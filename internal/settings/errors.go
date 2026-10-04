package settings

import "errors"

var (
	ErrConfig = errors.New("config error")
)

func configError(field, message string) error {
	return errors.Join(ErrConfig, errors.New("ConfigError: "+field+" - "+message))
}
