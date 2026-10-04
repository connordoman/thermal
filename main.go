package main

import (
	"errors"
	"io/fs"

	"github.com/connordoman/thermal/internal/console"
	"github.com/joho/godotenv"
)

func init() {
	if err := godotenv.Load(".env", ".env.local"); err != nil && errors.Is(err, fs.ErrNotExist) {
		console.Warn("no .env or .env.local file found")
	} else if err != nil {
		console.Error("failed to load .env or .env.local file: %v", err)
	}
}

func main() {
	console.Log("hello world")
	console.Debug("hello world")
	console.Info("hello world")
	console.Warn("hello world")
	console.Error("hello world")
	console.Fatal("hello world")
}
