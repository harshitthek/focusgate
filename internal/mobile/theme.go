package mobile

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// FocusGateTheme implements Fyne's Theme interface matching FocusGate's paper & tape aesthetic.
type FocusGateTheme struct{}

var _ fyne.Theme = (*FocusGateTheme)(nil)

// Brand Color Palette
var (
	ColorSignalOrange = color.NRGBA{R: 0xF0, G: 0x44, B: 0x00, A: 0xFF} // #F04400
	ColorTapeYellow   = color.NRGBA{R: 0xFF, G: 0xD9, B: 0x1A, A: 0xFF} // #FFD91A
	ColorInkBlack     = color.NRGBA{R: 0x0F, G: 0x0F, B: 0x0F, A: 0xFF} // #0F0F0F
	ColorPaperCream   = color.NRGBA{R: 0xF4, G: 0xF0, B: 0xE8, A: 0xFF} // #F4F0E8
	ColorPureWhite    = color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	ColorBorderDark   = color.NRGBA{R: 0x22, G: 0x22, B: 0x22, A: 0xFF}
	ColorMutedGray    = color.NRGBA{R: 0x77, G: 0x77, B: 0x77, A: 0xFF}
)

func (t *FocusGateTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return ColorPaperCream
	case theme.ColorNameButton:
		return ColorTapeYellow
	case theme.ColorNamePrimary:
		return ColorSignalOrange
	case theme.ColorNameForeground:
		return ColorInkBlack
	case theme.ColorNameInputBackground:
		return ColorPureWhite
	case theme.ColorNameShadow:
		return color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x40}
	default:
		return theme.DefaultTheme().Color(name, variant)
	}
}

func (t *FocusGateTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (t *FocusGateTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (t *FocusGateTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNamePadding:
		return 8
	case theme.SizeNameInlineIcon:
		return 20
	case theme.SizeNameText:
		return 14
	case theme.SizeNameHeadingText:
		return 20
	case theme.SizeNameSubHeadingText:
		return 16
	default:
		return theme.DefaultTheme().Size(name)
	}
}
