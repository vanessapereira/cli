package requirements

import (
	"errors"
	"fmt"

	. "github.com/cloudfoundry/cli/cf/i18n"
)

func NewUsageRequirement(cmd Usable, errorMessage string, pred func() bool) Requirement {
	return RequirementFunction(func() error {
		if pred() {
			m := fmt.Sprintf("%s. %s\n\n%s", T("Incorrect Usage"), errorMessage, cmd.Usage())

			return errors.New(m)
		}

		return nil
	})
}

type Usable interface {
	Usage() string
}
