package requirements

type RequirementFunction func() error

func (f RequirementFunction) Execute() error {
	return f()
}
