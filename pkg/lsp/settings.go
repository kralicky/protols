package lsp

type Settings struct {
	LogLevel   string             `mapstructure:"logLevel" json:"logLevel"`
	InlayHints InlayHintsSettings `mapstructure:"inlayHints" json:"inlayHints"`
}

type InlayHintsSettings struct {
	ExtensionTypes *bool `mapstructure:"extensionTypes" json:"extensionTypes"`
	Imports        *bool `mapstructure:"imports" json:"imports"`
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
