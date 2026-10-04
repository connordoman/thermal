package console

import (
	"fmt"
	"log"

	"charm.land/lipgloss/v2"
	"github.com/connordoman/windy"
)

type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
	LogLevelFatal
	LogLevelNone
)

var baseStyle = lipgloss.NewStyle().Padding(0, 1).Bold(true)

var (
	enabledLevels = map[LogLevel]bool{
		LogLevelDebug: true,
		LogLevelInfo:  true,
		LogLevelWarn:  true,
		LogLevelError: true,
		LogLevelFatal: true,
		LogLevelNone:  false,
	}

	levelStyles = map[LogLevel]lipgloss.Style{
		LogLevelDebug: baseStyle.Background(windy.Fuchsia600.Glossy()).Foreground(lipgloss.White),
		LogLevelInfo:  baseStyle.Background(windy.Blue600.Glossy()).Foreground(lipgloss.White),
		LogLevelWarn:  baseStyle.Background(windy.Yellow600.Glossy()).Foreground(lipgloss.White),
		LogLevelError: baseStyle.Background(windy.Red600.Glossy()).Foreground(lipgloss.White),
		LogLevelFatal: baseStyle.Background(windy.Rose600.Glossy()).Foreground(lipgloss.White).Underline(true),
		LogLevelNone:  baseStyle,
	}

	levelNames = map[LogLevel]string{
		LogLevelDebug: "DEBUG",
		LogLevelInfo:  "INFO",
		LogLevelWarn:  "WARN",
		LogLevelError: "ERROR",
		LogLevelFatal: "FATAL",
		LogLevelNone:  "NONE",
	}
)

func SetLogLevel(level LogLevel, enabled bool) {
	enabledLevels[level] = enabled
}

func wrapper(level LogLevel, format string, a ...any) {
	if enabled, ok := enabledLevels[level]; !ok || !enabled {
		return
	}

	style, ok := levelStyles[level]
	if !ok {
		style = baseStyle
	}

	badge := ""
	if level != LogLevelNone {
		badge = style.Render(fmt.Sprintf("%s", levelNames[level]))
	}

	log.Println(badge, fmt.Sprintf(format, a...))
}

func Debug(format string, a ...any) {
	wrapper(LogLevelDebug, format, a...)
}

func Info(format string, a ...any) {
	wrapper(LogLevelInfo, format, a...)
}

func Warn(format string, a ...any) {
	wrapper(LogLevelWarn, format, a...)
}

func Error(format string, a ...any) {
	wrapper(LogLevelError, format, a...)
}

func Fatal(format string, a ...any) {
	wrapper(LogLevelFatal, format, a...)
}

func Log(format string, a ...any) {
	wrapper(LogLevelNone, format, a...)
}
