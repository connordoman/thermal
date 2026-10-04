package settings

type Config interface {
	Load() error
	Validate() error
}

type Configs = map[string]Config

var settings = Configs{
	"printer": NewPrinterConfig(),
	"server":  NewServerConfig(),
}

type MappedSettings struct {
	Printer *PrinterConfig
	Server  *ServerConfig
}

var Global = &MappedSettings{
	Printer: settings["printer"].(*PrinterConfig),
	Server:  settings["server"].(*ServerConfig),
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
