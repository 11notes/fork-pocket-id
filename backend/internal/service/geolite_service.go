package service

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/pocket-id/pocket-id/backend/internal/common"
)

type GeoLiteService struct {
	httpClient      *http.Client
	disableUpdater  bool
	mutex           sync.RWMutex
	localIPv6Ranges []*net.IPNet
}

var localhostIPNets = []*net.IPNet{
	{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}, // 127.0.0.0/8
	{IP: net.IPv6loopback, Mask: net.CIDRMask(128, 128)},    // ::1/128
}

var privateLanIPNets = []*net.IPNet{
	{IP: net.IPv4(10, 0, 0, 0), Mask: net.CIDRMask(8, 32)},     // 10.0.0.0/8
	{IP: net.IPv4(172, 16, 0, 0), Mask: net.CIDRMask(12, 32)},  // 172.16.0.0/12
	{IP: net.IPv4(192, 168, 0, 0), Mask: net.CIDRMask(16, 32)}, // 192.168.0.0/16
}

var tailscaleIPNets = []*net.IPNet{
	{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}, // 100.64.0.0/10
}

// NewGeoLiteService initializes a new GeoLiteService instance and starts a goroutine to update the GeoLite2 City database.
func NewGeoLiteService(httpClient *http.Client) *GeoLiteService {
	service := &GeoLiteService{
		httpClient: httpClient,
	}

	if common.EnvConfig.MaxMindLicenseKey == "" && common.EnvConfig.GeoLiteDBUrl == common.MaxMindGeoLiteCityUrl {
		// Warn the user, and disable the periodic updater
		slog.Warn("MAXMIND_LICENSE_KEY environment variable is empty: the GeoLite2 City database won't be updated")
		service.disableUpdater = true
	}

	// Initialize IPv6 local ranges
	err := service.initializeIPv6LocalRanges()
	if err != nil {
		slog.Warn("Failed to initialize IPv6 local ranges", slog.Any("error", err))
	}

	return service
}

// initializeIPv6LocalRanges parses the LOCAL_IPV6_RANGES environment variable
func (s *GeoLiteService) initializeIPv6LocalRanges() error {
	rangesEnv := common.EnvConfig.LocalIPv6Ranges
	if rangesEnv == "" {
		return nil // No local IPv6 ranges configured
	}

	ranges := strings.Split(rangesEnv, ",")
	localRanges := make([]*net.IPNet, 0, len(ranges))

	for _, rangeStr := range ranges {
		rangeStr = strings.TrimSpace(rangeStr)
		if rangeStr == "" {
			continue
		}

		_, ipNet, err := net.ParseCIDR(rangeStr)
		if err != nil {
			return fmt.Errorf("invalid IPv6 range '%s': %w", rangeStr, err)
		}

		// Ensure it's an IPv6 range
		if ipNet.IP.To4() != nil {
			return fmt.Errorf("range '%s' is not a valid IPv6 range", rangeStr)
		}

		localRanges = append(localRanges, ipNet)
	}

	s.localIPv6Ranges = localRanges

	if len(localRanges) > 0 {
		slog.Info("Initialized IPv6 local ranges", slog.Int("count", len(localRanges)))
	}
	return nil
}

// isLocalIPv6 checks if the given IPv6 address is within any of the configured local ranges
func (s *GeoLiteService) isLocalIPv6(ip net.IP) bool {
	if ip.To4() != nil {
		return false // Not an IPv6 address
	}

	for _, localRange := range s.localIPv6Ranges {
		if localRange.Contains(ip) {
			return true
		}
	}

	return false
}

func (s *GeoLiteService) DisableUpdater() bool {
	return s.disableUpdater
}

// GetLocationByIP returns the country and city of the given IP address.
func (s *GeoLiteService) GetLocationByIP(ipAddress string) (country, city string, err error) {
	if ipAddress == "" {
		return "", "", nil
	}

	// Check the IP address against known private IP ranges
	if ip := net.ParseIP(ipAddress); ip != nil {
		// Check IPv6 local ranges first
		if s.isLocalIPv6(ip) {
			return "Internal Network", "LAN", nil
		}

		// Check existing IPv4 ranges
		for _, ipNet := range tailscaleIPNets {
			if ipNet.Contains(ip) {
				return "Internal Network", "Tailscale", nil
			}
		}
		for _, ipNet := range privateLanIPNets {
			if ipNet.Contains(ip) {
				return "Internal Network", "LAN", nil
			}
		}
		for _, ipNet := range localhostIPNets {
			if ipNet.Contains(ip) {
				return "Internal Network", "localhost", nil
			}
		}
	}

	addr, err := netip.ParseAddr(ipAddress)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse IP address: %w", err)
	}

	// Race condition between reading and writing the database.
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	db, err := maxminddb.Open(common.EnvConfig.GeoLiteDBPath)
	if err != nil {
		return "", "", err
	}
	defer db.Close()

	var record struct {
		City struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"city"`
		Country struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"country"`
	}

	err = db.Lookup(addr).Decode(&record)
	if err != nil {
		return "", "", err
	}

	return record.Country.Names["en"], record.City.Names["en"], nil
}

// UpdateDatabase checks the age of the database and updates it if it's older than 14 days.
func (s *GeoLiteService) UpdateDatabase(parentCtx context.Context) error {
	if s.isDatabaseUpToDate() {
		slog.Info("GeoLite2 City database is up-to-date")
		return nil
	}

	slog.Info("Updating GeoLite2 City database")
	downloadUrl := fmt.Sprintf(common.EnvConfig.GeoLiteDBUrl, common.EnvConfig.MaxMindLicenseKey)

	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadUrl, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download database: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download database, received HTTP %d", resp.StatusCode)
	}

	// Extract the database file directly to the target path
	err = s.extractDatabase(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to extract database: %w", err)
	}

	slog.Info("GeoLite2 City database successfully updated.")
	return nil
}

// isDatabaseUpToDate checks if the database file is older than 14 days.
func (s *GeoLiteService) isDatabaseUpToDate() bool {
	info, err := os.Stat(common.EnvConfig.GeoLiteDBPath)
	if err != nil {
		// If the file doesn't exist, treat it as not up-to-date
		return false
	}
	return time.Since(info.ModTime()) < 14*24*time.Hour
}

// extractDatabase extracts the database file from the tar.gz archive directly to the target location.
func (s *GeoLiteService) extractDatabase(reader io.Reader) error {
	gzr, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	tarReader := tar.NewReader(gzr)

	var totalSize int64
	const maxTotalSize = 300 * 1024 * 1024 // 300 MB limit for total decompressed size

	// Iterate over the files in the tar archive
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("failed to read tar archive: %w", err)
		}

		// Check if the file is the GeoLite2-City.mmdb file
		if header.Typeflag == tar.TypeReg && filepath.Base(header.Name) == "GeoLite2-City.mmdb" {
			totalSize += header.Size
			if totalSize > maxTotalSize {
				return errors.New("total decompressed size exceeds maximum allowed limit")
			}

			// extract to a temporary file to avoid having a corrupted db in case of write failure.
			baseDir := filepath.Dir(common.EnvConfig.GeoLiteDBPath)
			tmpFile, err := os.CreateTemp(baseDir, "geolite.*.mmdb.tmp")
			if err != nil {
				return fmt.Errorf("failed to create temporary database file: %w", err)
			}
			tempName := tmpFile.Name()

			// Write the file contents directly to the target location
			if _, err := io.Copy(tmpFile, tarReader); err != nil { //nolint:gosec
				// if fails to write, then cleanup and throw an error
				tmpFile.Close()
				os.Remove(tempName)
				return fmt.Errorf("failed to write database file: %w", err)
			}
			tmpFile.Close()

			// ensure the database is not corrupted
			db, err := maxminddb.Open(tempName)
			if err != nil {
				// if fails to write, then cleanup and throw an error
				os.Remove(tempName)
				return fmt.Errorf("failed to open downloaded database file: %w", err)
			}
			db.Close()

			// ensure we lock the structure before we overwrite the database
			// to prevent race conditions between reading and writing the mmdb.
			s.mutex.Lock()
			// replace the old file with the new file
			err = os.Rename(tempName, common.EnvConfig.GeoLiteDBPath)
			s.mutex.Unlock()

			if err != nil {
				// if cannot overwrite via rename, then cleanup and throw an error
				os.Remove(tempName)
				return fmt.Errorf("failed to replace database file: %w", err)
			}
			return nil
		}
	}

	return errors.New("GeoLite2-City.mmdb not found in archive")
}
