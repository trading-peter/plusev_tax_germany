package main

import (
	"encoding/json"

	"github.com/extism/go-pdk"
	"github.com/plusev-terminal/go-plugin-common/logging"
	m "github.com/plusev-terminal/go-plugin-common/meta"
	"github.com/plusev-terminal/go-plugin-common/plugin"
	"github.com/plusev-terminal/go-plugin-common/tax"
)

var pluginInstance = &GermanTaxPlugin{}

func init() {
	plugin.RegisterPlugin(pluginInstance)
}

var log = logging.NewLogger("german-tax-report")

func main() {}

// GermanTaxPlugin implements the plugin.Plugin interface for generating
// German tax reports according to BMF-Schreiben (§23 EStG).
type GermanTaxPlugin struct {
	config *plugin.ConfigStore
}

func (p *GermanTaxPlugin) GetMeta() m.Meta {
	return m.Meta{
		PluginID:    "german-tax-report",
		Name:        "German Tax Report",
		AppID:       "plusev_taxes",
		Category:    "Report",
		Description: "Erstellt deutsche Steuerberichte für Kryptowährungen nach §23 EStG mit FIFO-Verfahren, Haltefristregelung und Freigrenze.",
		Author:      "trading_peter",
		Version:     "v0.1.0",
		Tags:        []string{"german", "tax", "report", "fifo", "bmf"},
		Features:    []string{},
		Resources: m.ResourceAccess{
			FsWriteAccess: map[string]string{
				"templates": "/templates",
			},
		},
	}
}

func (p *GermanTaxPlugin) GetRateLimits() []plugin.RateLimit {
	return nil
}

func (p *GermanTaxPlugin) GetConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{}
}

func (p *GermanTaxPlugin) OnInit(config *plugin.ConfigStore) error {
	p.config = config
	return nil
}

func (p *GermanTaxPlugin) OnShutdown() error {
	return nil
}

func (p *GermanTaxPlugin) RegisterCommands(router *plugin.CommandRouter) {
	router.Register(tax.CMD_GENERATE_REPORT, p.handleGenerateReport)
}

//go:wasmexport get_report_metadata
func get_report_metadata() int32 {
	pdk.OutputJSON(map[string]any{
		"name":        "German Tax Report",
		"country":     "DE",
		"description": "Erstellt deutsche Steuerberichte für Kryptowährungen nach §23 EStG mit FIFO-Verfahren, Haltefristregelung und Freigrenze.",
		"tax_laws":    []string{"§23 EStG", "§20 EStG", "§22 Nr. 3 EStG"},
	})
	return 0
}

//go:wasmexport generate_report
func generate_report() int32 {
	var params map[string]any
	if err := json.Unmarshal(pdk.Input(), &params); err != nil {
		return plugin.WriteResponse(plugin.ErrorResponse(err))
	}
	return plugin.WriteResponse(pluginInstance.handleGenerateReport(params))
}
