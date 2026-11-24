package mobile

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

// UI represents the visual mobile interface.
type UI struct {
	engine       *Engine
	window       fyne.Window
	statusText   *canvas.Text
	modeLabel    *widget.Label
	itemList     *widget.List
	items        []store.StoredItem
	itemsAccount accountSnapshot
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
			if err := ui.engine.SyncWithServer(context.Background()); err != nil {
				ui.reportError("FocusGate Sync Failed", err)
			}
		}()
	})
	syncBtn.Importance = widget.HighImportance

	fetchBtn := widget.NewButton("CELLULAR FETCH", func() {
		go func() {
			ui.engine.DirectCellularFetch(context.Background())
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
			account := ui.itemsAccount
			vbox := obj.(*fyne.Container)
			titleLbl := vbox.Objects[0].(*widget.Label)
			snippetLbl := vbox.Objects[1].(*widget.Label)
			actionBox := vbox.Objects[2].(*fyne.Container)
			tagLbl := actionBox.Objects[0].(*widget.Label)
			readBtn := actionBox.Objects[2].(*widget.Button)
			openBtn := actionBox.Objects[3].(*widget.Button)

			titleLbl.SetText(item.Title)
			body := item.Body
			if len([]rune(body)) > 120 {
				body = ingest.TruncateRunes(body, 117) + "..."
			}
			snippetLbl.SetText(body)

			tagLbl.SetText(fmt.Sprintf("[%s] %s", item.SourceType, item.Tag))

			readBtn.OnTapped = func() {
				if !ui.engine.accountIsCurrent(account) {
					return
				}
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
					defer cancel()
					if err := ui.engine.markRead(ctx, account, item.ID); err != nil && ui.engine.accountIsCurrent(account) {
						ui.reportError("FocusGate Archive Failed", err)
					}
				}()
			}

			openBtn.OnTapped = func() {
				if !ui.engine.accountIsCurrent(account) {
					return
				}
				if item.URL != "" {
					u, err := url.Parse(item.URL)
					if err == nil && u.Hostname() != "" && (u.Scheme == "https" || u.Scheme == "http") {
						_ = fyne.CurrentApp().OpenURL(u)
					}
				}
			}
		},
	)

	// 5. Settings Tab View
	serverInput := widget.NewEntry()
	serverInput.SetText(engine.Config().ServerURL)

	usernameInput := widget.NewEntry()
	usernameInput.SetText(engine.Config().Username)
	usernameInput.SetPlaceHolder("Username on sandoxed.xyz")

	passwordInput := widget.NewPasswordEntry()
	passwordInput.SetPlaceHolder("Account password (for logging in to account)")

	tokenInput := widget.NewPasswordEntry()
	tokenInput.SetText(engine.Config().AuthToken)
	tokenInput.SetPlaceHolder("Session token or API key (auto-filled on login)")

	pollInput := widget.NewEntry()
	pollInput.SetText(strconv.Itoa(engine.Config().PollMinutes))

	autoOfflineCheck := widget.NewCheck("Auto-switch to Cellular Direct when offline", nil)
	autoOfflineCheck.SetChecked(engine.Config().AutoOffline)

	var saveSettingsBtn *widget.Button
	saveSettingsBtn = widget.NewButton("SAVE SETTINGS", func() {
		pollMin, _ := strconv.Atoi(pollInput.Text)
		sURL := strings.TrimRight(strings.TrimSpace(serverInput.Text), "/")
		uName := strings.TrimSpace(usernameInput.Text)
		pwd := passwordInput.Text
		tok := strings.TrimSpace(tokenInput.Text)
		autoOffline := autoOfflineCheck.Checked

		if sURL != "" && uName != "" && pwd != "" {
			account := engine.account()
			saveSettingsBtn.Disable()
			go func() {
				defer fyne.Do(func() {
					saveSettingsBtn.Enable()
				})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				committed, err := engine.loginWithServer(ctx, account, sURL, uName, pwd, autoOffline, pollMin)
				cancel()
				if err != nil {
					fyne.Do(func() {
						ui.engine.app.SendNotification(&fyne.Notification{
							Title:   "FocusGate Login Failed",
							Content: fmt.Sprintf("Authentication error: %v", err),
						})
					})
					return
				}
				fyne.Do(func() {
					if !engine.accountIsCurrent(committed) {
						return
					}
					tokenInput.SetText(committed.AuthToken)
					usernameInput.SetText(committed.Username)
					serverInput.SetText(committed.ServerURL)
					pollInput.SetText(strconv.Itoa(committed.PollMinutes))
					autoOfflineCheck.SetChecked(committed.AutoOffline)
					passwordInput.SetText("")
					ui.engine.app.SendNotification(&fyne.Notification{
						Title:   "FocusGate Connected",
						Content: fmt.Sprintf("Logged in as @%s", uName),
					})
				})
			}()
			return
		}

		if err := engine.UpdateUserConfig(sURL, uName, tok, autoOffline, pollMin); err != nil {
			ui.engine.app.SendNotification(&fyne.Notification{
				Title:   "FocusGate Settings Error",
				Content: err.Error(),
			})
			return
		}
		ui.engine.app.SendNotification(&fyne.Notification{
			Title:   "FocusGate Settings",
			Content: "Configuration updated successfully.",
		})
	})
	saveSettingsBtn.Importance = widget.HighImportance

	settingsForm := widget.NewForm(
		widget.NewFormItem("Server URL", serverInput),
		widget.NewFormItem("Username", usernameInput),
		widget.NewFormItem("Password", passwordInput),
		widget.NewFormItem("Session Token", tokenInput),
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
		fyne.Do(func() {
			ui.statusText.Text = string(ui.engine.Status())
			ui.statusText.Refresh()
			ui.refreshItems()
		})
	})

	return container.NewBorder(
		topBar,
		nil, nil, nil,
		tabs,
	)
}

func (ui *UI) setFocusMode(mode string) {
	account := ui.engine.account()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := ui.engine.setFocusMode(ctx, account, config.FocusMode(mode)); err != nil && ui.engine.accountIsCurrent(account) {
			ui.reportError("FocusGate Mode Update Failed", err)
		}
	}()
}

func (ui *UI) reportError(title string, err error) {
	fyne.Do(func() {
		ui.engine.app.SendNotification(&fyne.Notification{Title: title, Content: err.Error()})
	})
}

func (ui *UI) refreshItems() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !ui.engine.accountIsCurrent(ui.itemsAccount) {
		ui.items = nil
		ui.modeLabel.SetText("MODE: —")
		if ui.itemList != nil {
			ui.itemList.Refresh()
		}
	}
	account, mode, items, err := ui.engine.inboxRows(ctx)
	if err == nil && ui.engine.accountIsCurrent(account) {
		ui.items = items
		ui.itemsAccount = account
		ui.modeLabel.SetText("MODE: " + strings.ToUpper(string(mode)))
		if ui.itemList != nil {
			ui.itemList.Refresh()
		}
	}
}
