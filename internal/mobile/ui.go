package mobile

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/store"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

// UI represents the visual mobile interface.
type UI struct {
	engine     *Engine
	window     fyne.Window
	statusText *canvas.Text
	modeLabel  *widget.Label
	itemList   *widget.List
	items      []store.StoredItem
}

// BuildUI constructs the complete FocusGate mobile window and layout.
func BuildUI(engine *Engine, w fyne.Window) fyne.CanvasObject {
	ui := &UI{
		engine: engine,
		window: w,
	}

	// 1. Header with Brand & Live Connectivity Pill
	title := canvas.NewText("FOCUS GATE", ColorInkBlack)
	title.TextStyle = fyne.TextStyle{Bold: true}
	title.TextSize = 22

	subtitle := canvas.NewText("MOBILE FEED ROUTER", ColorSignalOrange)
	subtitle.TextStyle = fyne.TextStyle{Bold: true}
	subtitle.TextSize = 10

	titleBox := container.NewVBox(title, subtitle)

	ui.statusText = canvas.NewText(string(engine.Status()), ColorSignalOrange)
	ui.statusText.TextStyle = fyne.TextStyle{Bold: true}
	ui.statusText.TextSize = 12

	topBar := container.NewHBox(
		titleBox,
		layout.NewSpacer(),
		ui.statusText,
	)

	// 2. Retro Pager Gadget (Focus Mode Switcher)
	ui.modeLabel = widget.NewLabelWithStyle("MODE: NORMAL", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})

	btnNormal := widget.NewButton("NORMAL", func() {
		ui.setFocusMode("normal")
	})
	btnDeepWork := widget.NewButton("DEEP WORK", func() {
		ui.setFocusMode("deep_work")
	})
	btnDND := widget.NewButton("DND", func() {
		ui.setFocusMode("dnd")
	})

	modeButtons := container.NewGridWithColumns(3, btnNormal, btnDeepWork, btnDND)
	pagerCard := widget.NewCard("RETRO PAGER", "", container.NewVBox(ui.modeLabel, modeButtons))

	// 3. Action Buttons (Manual Sync & Direct Cellular Fetch)
	syncBtn := widget.NewButton("SYNC SERVER", func() {
		go func() {
			_ = ui.engine.SyncWithServer(context.Background())
			ui.refreshItems()
		}()
	})
	syncBtn.Importance = widget.HighImportance

	fetchBtn := widget.NewButton("CELLULAR FETCH", func() {
		go func() {
			ui.engine.DirectCellularFetch(context.Background())
			ui.refreshItems()
		}()
	})

	actionRow := container.NewGridWithColumns(2, syncBtn, fetchBtn)

	// 4. Notification / Digest Items List
	ui.refreshItems()

	ui.itemList = widget.NewList(
		func() int {
			return len(ui.items)
		},
		func() fyne.CanvasObject {
			return container.NewVBox(
				widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				widget.NewLabel(""),
				container.NewHBox(
					widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
					layout.NewSpacer(),
					widget.NewButton("MARK READ", nil),
					widget.NewButton("OPEN LINK", nil),
				),
			)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			if id >= len(ui.items) {
				return
			}
			item := ui.items[id]
			vbox := obj.(*fyne.Container)
			titleLbl := vbox.Objects[0].(*widget.Label)
			snippetLbl := vbox.Objects[1].(*widget.Label)
			actionBox := vbox.Objects[2].(*fyne.Container)
			tagLbl := actionBox.Objects[0].(*widget.Label)
			readBtn := actionBox.Objects[2].(*widget.Button)
			openBtn := actionBox.Objects[3].(*widget.Button)

			titleLbl.SetText(item.Title)
			body := item.Body
			if len(body) > 120 {
				body = body[:117] + "..."
			}
			snippetLbl.SetText(body)

			tagLbl.SetText(fmt.Sprintf("[%s] %s", item.SourceType, item.Tag))

			readBtn.OnTapped = func() {
				_ = ui.engine.MarkRead(context.Background(), item.ID)
				ui.refreshItems()
			}

			openBtn.OnTapped = func() {
				if item.URL != "" {
					u, err := url.Parse(item.URL)
					if err == nil {
						_ = fyne.CurrentApp().OpenURL(u)
					}
				}
			}
		},
	)

	// 5. Settings Tab View
	serverInput := widget.NewEntry()
	serverInput.SetText(engine.Config().ServerURL)

	tokenInput := widget.NewPasswordEntry()
	tokenInput.SetText(engine.Config().AuthToken)

	pollInput := widget.NewEntry()
	pollInput.SetText(strconv.Itoa(engine.Config().PollMinutes))

	autoOfflineCheck := widget.NewCheck("Auto-switch to Cellular Direct when offline", nil)
	autoOfflineCheck.SetChecked(engine.Config().AutoOffline)

	saveSettingsBtn := widget.NewButton("SAVE SETTINGS", func() {
		pollMin, _ := strconv.Atoi(pollInput.Text)
		_ = engine.UpdateConfig(serverInput.Text, tokenInput.Text, autoOfflineCheck.Checked, pollMin)
		ui.engine.app.SendNotification(&fyne.Notification{
			Title:   "FocusGate Settings",
			Content: "Mobile configuration updated successfully.",
		})
	})
	saveSettingsBtn.Importance = widget.HighImportance

	settingsForm := widget.NewForm(
		widget.NewFormItem("Server URL", serverInput),
		widget.NewFormItem("Auth Token / Passphrase", tokenInput),
		widget.NewFormItem("Poll Interval (mins)", pollInput),
	)

	settingsTab := container.NewVBox(
		settingsForm,
		autoOfflineCheck,
		saveSettingsBtn,
	)

	// App Tabs (Main Queue vs Settings)
	queueTab := container.NewBorder(
		container.NewVBox(pagerCard, actionRow),
		nil, nil, nil,
		ui.itemList,
	)

	tabs := container.NewAppTabs(
		container.NewTabItem("QUEUE & FEED", queueTab),
		container.NewTabItem("SETTINGS", settingsTab),
	)

	// Register reactive listener from engine
	engine.SetOnStateChange(func() {
		ui.statusText.Text = string(ui.engine.Status())
		ui.statusText.Refresh()
		ui.refreshItems()
	})

	return container.NewBorder(
		topBar,
		nil, nil, nil,
		tabs,
	)
}

func (ui *UI) setFocusMode(mode string) {
	ui.modeLabel.SetText("MODE: " + mode)
	_ = ui.engine.Store().SetFocusMode(context.Background(), config.FocusMode(mode))

	go func() {
		serverURL := ui.engine.Config().ServerURL
		token := ui.engine.Config().AuthToken
		if serverURL == "" || token == "" {
			return
		}
		patchURL := fmt.Sprintf("%s/api/focus?mode=%s", serverURL, mode)
		req, _ := http.NewRequest(http.MethodPost, patchURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		client := &http.Client{Timeout: 5 * time.Second}
		_, _ = client.Do(req)
	}()
}

func (ui *UI) refreshItems() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	filter := store.ItemFilter{
		Limit: 50,
	}
	items, err := ui.engine.Store().ListItems(ctx, filter)
	if err == nil {
		ui.items = items
		if ui.itemList != nil {
			ui.itemList.Refresh()
		}
	}
}
