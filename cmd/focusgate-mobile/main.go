package main

import (
	"context"
	"log"

	"focusgate/internal/mobile"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
)

func main() {
	// Initialize native Fyne application with official package ID
	a := app.NewWithID("xyz.sandoxed.focusgate")
	a.Settings().SetTheme(&mobile.FocusGateTheme{})

	w := a.NewWindow("FocusGate — Mobile Signal Router")
	w.Resize(fyne.NewSize(390, 780)) // Standard modern mobile resolution

	// Initialize local-first engine
	engine, err := mobile.NewEngine(a)
	if err != nil {
		log.Fatalf("Failed to initialize mobile engine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start sync and cellular direct polling loop
	engine.StartBackgroundLoop(ctx)

	// Build and display UI
	rootUI := mobile.BuildUI(engine, w)
	w.SetContent(rootUI)

	w.ShowAndRun()
}
