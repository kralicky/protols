package lsp

type Settings struct {
	InlayHints InlayHintsSettings `mapstructure:"inlayHints"`
}

type InlayHintsSettings struct {
	ExtensionTypes *bool `mapstructure:"extensionTypes"`
	Imports        *bool `mapstructure:"imports"`
}

func (s *InlayHintsSettings) GetExtensionTypes() bool {
	if s.ExtensionTypes == nil {
		return true
	}
	return *s.ExtensionTypes
}

func (s *InlayHintsSettings) GetImports() bool {
	if s.Imports == nil {
		return true
	}
	return *s.Imports
}
