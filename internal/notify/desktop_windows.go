//go:build windows

// Package notify provides platform notification dispatchers.
package notify

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"time"
	"unicode/utf16"

	"focusgate/internal/ingest"
)

// SendDesktopToast displays a native Windows 10/11 toast notification on demand
// without keeping a heavy GUI framework resident in RAM.
func SendDesktopToast(ctx context.Context, title, body string) error {
	cleanTitle := ingest.TruncateRunes(title, 100)
	cleanBody := ingest.TruncateRunes(body, 240)

	const psScript = `
$ErrorActionPreference = 'Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml('<toast><visual><binding template="ToastText02"><text id="1"></text><text id="2"></text></binding></visual></toast>')
$xml.SelectSingleNode('//text[@id="1"]').AppendChild($xml.CreateTextNode($env:FOCUSGATE_TOAST_TITLE)) | Out-Null
$xml.SelectSingleNode('//text[@id="2"]').AppendChild($xml.CreateTextNode($env:FOCUSGATE_TOAST_BODY)) | Out-Null
$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('FocusGate').Show($toast)
`

	runCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	encoded := encodeUTF16LEBase64(psScript)
	cmd := exec.CommandContext(runCtx, "powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", encoded)
	cmd.Env = append(os.Environ(),
		"FOCUSGATE_TOAST_TITLE="+cleanTitle,
		"FOCUSGATE_TOAST_BODY="+cleanBody,
	)
	return cmd.Run()
}

func encodeUTF16LEBase64(s string) string {
	u16 := utf16.Encode([]rune(s))
	bytes := make([]byte, len(u16)*2)
	for i, v := range u16 {
		bytes[i*2] = byte(v)
		bytes[i*2+1] = byte(v >> 8)
	}
	return base64.StdEncoding.EncodeToString(bytes)
}
