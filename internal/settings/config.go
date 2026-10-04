package settings

type Config interface {
	Load() error
	Validate() error
}

type Configs = map[string]Config

var settings = Configs{
	"printer": NewPrinterConfig(),
}

type MappedSettings struct {
	Printer *PrinterConfig
}

var Global = &MappedSettings{
	Printer: settings["printer"].(*PrinterConfig),
}

func Load() []error {
	var errors []error
	for _, config := range settings {
		if err := config.Load(); err != nil {
			errors = append(errors, err)
		}
	}
	return errors
}

func Validate() []error {
	var errors []error
	for _, config := range settings {
		if err := config.Validate(); err != nil {
			errors = append(errors, err)
		}
	}
	return errors
}
