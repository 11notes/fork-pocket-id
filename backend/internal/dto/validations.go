package dto

import (
	"log/slog"
	"os"
	"regexp"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

// [a-zA-Z0-9]      : The username must start with an alphanumeric character
// [a-zA-Z0-9_.@-]* : The rest of the username can contain alphanumeric characters, dots, underscores, hyphens, and "@" symbols
// [a-zA-Z0-9]$     : The username must end with an alphanumeric character
var validateUsernameRegex = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9_.@-]*[a-zA-Z0-9]$")

var validateUsername validator.Func = func(fl validator.FieldLevel) bool {
	return validateUsernameRegex.MatchString(fl.Field().String())
}

func init() {
	v, _ := binding.Validator.Engine().(*validator.Validate)
	err := v.RegisterValidation("username", validateUsername)
	if err != nil {
		slog.Error("Failed to register custom validation", slog.Any("error", err))
		os.Exit(1)
		return
	}
}
