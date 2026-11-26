package main

import (
	"fmt"
	"io/fs"
	"os"

	"tokenanalyse/internal/server"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

func main() {
	assets, err := fs.Sub(server.WebFS(), "web")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	app := NewApp()
	err = wails.Run(&options.App{
		Title:     "TokenAnalyse",
		Width:     1280,
		Height:    800,
		MinWidth:  960,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: app.backendHandler(),
		},
		BackgroundColour: &options.RGBA{R: 11, G: 13, B: 18, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []any{app},
		Mac:              &mac.Options{TitleBar: mac.TitleBarHiddenInset()},
		Windows:          &windows.Options{WebviewIsTransparent: false},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
