// Package desktop adapts the native tray and browser without starting either
// during package initialization. Call New and Run on the main OS thread.
package desktop

import (
	_ "embed"
	"sync"

	"github.com/gogpu/systray"
)

type Tray struct {
	native  *systray.SystemTray
	once    sync.Once
	cleanup func()
}

//go:embed assets/icon.png
var iconPNG []byte

//go:embed assets/template.png
var templatePNG []byte

func New(open, quit func()) (*Tray, error) {
	if err := trayAvailable(); err != nil {
		return nil, err
	}
	tray := systray.New()
	menu := systray.NewMenu()
	menu.Add("Open", open)
	menu.AddSeparator()
	menu.Add("Exit", quit)
	tray.SetAppName("PKUDisk").SetTooltip("PKUDisk").
		SetIcon(iconPNG).SetTemplateIcon(templatePNG).SetMenu(menu)
	tray.OnClick(open)
	tray.Show()
	cleanup, err := installReopen(open)
	if err != nil {
		tray.Remove()
		return nil, err
	}
	return &Tray{native: tray, cleanup: cleanup}, nil
}

func (t *Tray) Run() error {
	defer t.cleanup()
	return t.native.Run()
}

func (t *Tray) Close() { t.once.Do(t.native.Remove) }
