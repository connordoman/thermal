package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"

	"github.com/connordoman/thermal/internal/console"
	"github.com/connordoman/thermal/internal/settings"
	"github.com/connordoman/thermal/internal/thermal"
	"github.com/joho/godotenv"
)

func init() {
	if err := godotenv.Load(); err != nil && errors.Is(err, fs.ErrNotExist) {
		console.Warn("no .env or .env.local file found")
		log.Println(err)
	} else if err != nil {
		console.Error("failed to load .env or .env.local file: %v", err)
	}

	if errs := settings.Load(); len(errs) > 0 {
		console.Fatal("failed to load settings: %d error(s): %v", len(errs), errs)
	}
}

func main() {
	printer, close, err := thermal.GetPrinter()
	if err != nil {
		console.Fatal("failed to get printer: %v", err)
		os.Exit(1)
		return
	}
	defer close()

	thermal.RenderUTF8(printer, "hello world")

	printer.Flush(context.Background())

	printer.FeedAndCut(50)
	printer.Flush(context.Background())
}
