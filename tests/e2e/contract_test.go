package e2e

import (
	"sync"
	"testing"

	"github.com/rtrtarun/taskify/contract/contracttest"
)

var (
	validatorOnce sync.Once
	validator     *contracttest.Validator
	validatorErr  error
)

// contractValidator returns the response validator, loaded once.
func contractValidator(t *testing.T) *contracttest.Validator {
	t.Helper()
	validatorOnce.Do(func() { validator, validatorErr = contracttest.NewValidator() })
	if validatorErr != nil {
		t.Fatal(validatorErr)
	}
	return validator
}
