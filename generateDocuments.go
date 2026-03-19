package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/plusev-terminal/go-plugin-common/tax"
	"github.com/shopspring/decimal"
	"github.com/trading-peter/plusev_tax_germany/taxcalc"
)

// ──────────────────────────────────────────────────────────────────────────────
// Account labels
// ──────────────────────────────────────────────────────────────────────────────
// Map raw account identifiers to human-readable labels.
// If an account is not listed here, the raw name is used as-is.
var accountLabels = map[string]string{
	// "kraken_sub1": "Kraken",
	// "ledger_0x7798": "ETH Ledger 7798",
}

func accountLabel(account string) string {
	if label, ok := accountLabels[account]; ok {
		return label
	}
	return account
}

// ──────────────────────────────────────────────────────────────────────────────
// Aggregation types
// ──────────────────────────────────────────────────────────────────────────────

type categoryStats struct {
	Count    int
	PnL      decimal.Decimal
	Gains    decimal.Decimal // sum of positive PnLs
	Losses   decimal.Decimal // sum of negative PnLs
	Cost     decimal.Decimal // Anschaffungskosten (CostBasis from entries)
	Proceeds decimal.Decimal // Veräußerungserlös
	Fees     decimal.Decimal // disposal-side fees
}

func (s categoryStats) Turnover() decimal.Decimal {
	return s.Cost.Add(s.Proceeds)
}

type accountAgg struct {
	Account string
	Label   string
	Spot    categoryStats
	Deriv   categoryStats
	Staking categoryStats
	Airdrop categoryStats
}

type globalAgg struct {
	Spot    categoryStats
	Deriv   categoryStats
	Staking categoryStats
	Airdrop categoryStats
}

// docEntry holds a single entry decorated for per-account transaction tables.
type docEntry struct {
	Ts        time.Time
	Ticker    string
	Typ       string // "long", "short", "fee", "income"
	TxDesc    string // "SOL => CROWN"
	CostBasis decimal.Decimal
	Proceeds  decimal.Decimal
	FeeC      decimal.Decimal
	PnL       decimal.Decimal
	Category  string // spot, deriv, staking, airdrop
}

// ──────────────────────────────────────────────────────────────────────────────
// Aggregation
// ──────────────────────────────────────────────────────────────────────────────

func classifyEntry(e taxcalc.RecordEntry) string {
	switch {
	case e.TransferCategory == "staking_reward" || e.TransferCategory == "mining":
		return "staking"
	case e.TransferCategory == "airdrop" || e.TransferCategory == "airdrop_exempt":
		return "airdrop"
	case e.IsDerivative:
		return "deriv"
	default:
		return "spot"
	}
}

func addToStats(s *categoryStats, e taxcalc.RecordEntry) {
	s.Count++
	s.PnL = s.PnL.Add(e.PnL)

	if e.PnL.IsPositive() {
		s.Gains = s.Gains.Add(e.PnL)
	} else if e.PnL.IsNegative() {
		s.Losses = s.Losses.Add(e.PnL)
	}

	cb, _ := decimal.NewFromString(e.Entry.CostBasis)
	pr, _ := decimal.NewFromString(e.Entry.Proceeds)

	s.Cost = s.Cost.Add(cb)
	s.Proceeds = s.Proceeds.Add(pr)
	s.Fees = s.Fees.Add(e.FeeC)
}

func buildAggregation(entries []taxcalc.RecordEntry) (globalAgg, map[string]*accountAgg) {
	var g globalAgg
	accounts := make(map[string]*accountAgg)

	for _, e := range entries {
		cat := classifyEntry(e)

		// Ensure account aggregation exists.
		acc, ok := accounts[e.Account]
		if !ok {
			acc = &accountAgg{
				Account: e.Account,
				Label:   accountLabel(e.Account),
			}
			accounts[e.Account] = acc
		}

		switch cat {
		case "spot":
			addToStats(&g.Spot, e)
			addToStats(&acc.Spot, e)
		case "deriv":
			addToStats(&g.Deriv, e)
			addToStats(&acc.Deriv, e)
		case "staking":
			addToStats(&g.Staking, e)
			addToStats(&acc.Staking, e)
		case "airdrop":
			addToStats(&g.Airdrop, e)
			addToStats(&acc.Airdrop, e)
		}
	}

	return g, accounts
}

func buildDocEntries(entries []taxcalc.RecordEntry, account string) (spotEntries, derivEntries []docEntry) {
	for _, e := range entries {
		if e.Account != account {
			continue
		}

		cat := classifyEntry(e)
		cb, _ := decimal.NewFromString(e.Entry.CostBasis)
		pr, _ := decimal.NewFromString(e.Entry.Proceeds)

		ts, _ := time.Parse(time.RFC3339, e.Entry.Ts)

		typ := entryTypLabel(e)
		txDesc := entryTxDesc(e)

		de := docEntry{
			Ts:        ts,
			Ticker:    e.Ticker,
			Typ:       typ,
			TxDesc:    txDesc,
			CostBasis: cb,
			Proceeds:  pr,
			FeeC:      e.FeeC,
			PnL:       e.PnL,
			Category:  cat,
		}

		switch cat {
		case "deriv":
			derivEntries = append(derivEntries, de)
		default:
			spotEntries = append(spotEntries, de)
		}
	}

	return
}

func entryTypLabel(e taxcalc.RecordEntry) string {
	if e.Entry.RecordType == "transfer" {
		if e.Entry.TaxCategory == taxcalc.CatSonstigeEinkuenfte {
			return "Einnahme"
		}
		return "Gebühr"
	}

	if e.IsDerivative {
		if e.Action == 0 {
			return "long"
		}
		return "short"
	}

	if e.Action == 0 {
		return "long"
	}
	return "short"
}

func entryTxDesc(e taxcalc.RecordEntry) string {
	if e.Entry.RecordType == "transfer" {
		if e.Entry.TaxCategory == taxcalc.CatSonstigeEinkuenfte {
			return e.TransferCategory
		}
		return "Transfergebühr"
	}

	asset := e.Entry.Asset
	quote := e.Quote

	if quote == "" {
		quote = "?"
	}

	// BUY: spent quote to get asset → "quote => asset"
	// SELL: spent asset to get quote → "asset => quote"
	if e.Action == 0 {
		return quote + " => " + asset
	}
	return asset + " => " + quote
}

// ──────────────────────────────────────────────────────────────────────────────
// Template types
// ──────────────────────────────────────────────────────────────────────────────

type summaryData struct {
	TaxYear  int
	Spot     categoryStats
	Deriv    categoryStats
	Staking  categoryStats
	Airdrop  categoryStats
	Accounts []*accountAgg
}

type accountDocData struct {
	TaxYear      int
	Label        string
	Account      *accountAgg
	SpotEntries  []docEntry
	DerivEntries []docEntry
}

// ──────────────────────────────────────────────────────────────────────────────
// Template loading
// ──────────────────────────────────────────────────────────────────────────────

const templateDir = "/templates"

var tmplFuncs = template.FuncMap{
	"eur":        eur,
	"totalClass": totalClass,
	"notZero":    func(d decimal.Decimal) bool { return !d.IsZero() },
	"fmtDate":    func(t time.Time) string { return t.Format("02.01.2006 - 15:04:05") },
}

func loadTemplate(name string) (*template.Template, error) {
	content, err := os.ReadFile(templateDir + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("failed to read template %s: %w", name, err)
	}

	tmpl, err := template.New(name).Funcs(tmplFuncs).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template %s: %w", name, err)
	}

	return tmpl, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Document generation entry point
// ──────────────────────────────────────────────────────────────────────────────

const kvNamespace = "report_documents"

type docIndex struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

func generateDocuments(entries []taxcalc.RecordEntry, taxYear int, currency string, runID int) error {
	// Load templates from the sandboxed filesystem.
	summaryTmpl, err := loadTemplate("summary.html")
	if err != nil {
		return err
	}

	accountTmpl, err := loadTemplate("account.html")
	if err != nil {
		return err
	}

	g, accounts := buildAggregation(entries)
	prefix := fmt.Sprintf("%d_", runID)

	// Sort accounts deterministically.
	sortedAccounts := make([]*accountAgg, 0, len(accounts))
	for _, a := range accounts {
		sortedAccounts = append(sortedAccounts, a)
	}

	sort.Slice(sortedAccounts, func(i, j int) bool {
		return sortedAccounts[i].Label < sortedAccounts[j].Label
	})

	// Build document index.
	index := []docIndex{
		{Key: prefix + "summary", Title: fmt.Sprintf("Gesamtbericht %d", taxYear)},
	}

	for _, acc := range sortedAccounts {
		index = append(index, docIndex{
			Key:   prefix + "account_" + acc.Account,
			Title: fmt.Sprintf("Bericht %d — %s", taxYear, acc.Label),
		})
	}

	// Store index.
	idxJSON, _ := json.Marshal(index)
	if err := tax.KVPut(kvNamespace, prefix+"index", idxJSON); err != nil {
		return fmt.Errorf("failed to store document index: %w", err)
	}

	// Generate and store summary document.
	var summaryBuf bytes.Buffer
	err = summaryTmpl.Execute(&summaryBuf, summaryData{
		TaxYear:  taxYear,
		Spot:     g.Spot,
		Deriv:    g.Deriv,
		Staking:  g.Staking,
		Airdrop:  g.Airdrop,
		Accounts: sortedAccounts,
	})
	if err != nil {
		return fmt.Errorf("failed to render summary template: %w", err)
	}

	if err := tax.KVPut(kvNamespace, prefix+"summary", summaryBuf.Bytes()); err != nil {
		return fmt.Errorf("failed to store summary document: %w", err)
	}

	// Generate and store per-account documents.
	for _, acc := range sortedAccounts {
		spotEntries, derivEntries := buildDocEntries(entries, acc.Account)

		// Sort entries by timestamp.
		sort.Slice(spotEntries, func(i, j int) bool { return spotEntries[i].Ts.Before(spotEntries[j].Ts) })
		sort.Slice(derivEntries, func(i, j int) bool { return derivEntries[i].Ts.Before(derivEntries[j].Ts) })

		var buf bytes.Buffer
		err = accountTmpl.Execute(&buf, accountDocData{
			TaxYear:      taxYear,
			Label:        acc.Label,
			Account:      acc,
			SpotEntries:  spotEntries,
			DerivEntries: derivEntries,
		})
		if err != nil {
			log.WarnWithData("Failed to render account template", map[string]any{
				"account": acc.Account,
				"error":   err.Error(),
			})
			continue
		}

		key := prefix + "account_" + acc.Account
		if err := tax.KVPut(kvNamespace, key, buf.Bytes()); err != nil {
			log.WarnWithData("Failed to store account document", map[string]any{
				"account": acc.Account,
				"error":   err.Error(),
			})
		}
	}

	log.InfoWithData("Report documents generated", map[string]any{
		"taxYear":   taxYear,
		"documents": len(index),
	})

	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Formatting helpers (used by templates via FuncMap)
// ──────────────────────────────────────────────────────────────────────────────

func eur(d decimal.Decimal) string {
	s := d.StringFixed(2)

	// German number formatting: dot for thousands, comma for decimal.
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	decPart := "00"
	if len(parts) > 1 {
		decPart = parts[1]
	}

	negative := false
	if strings.HasPrefix(intPart, "-") {
		negative = true
		intPart = intPart[1:]
	}

	// Insert thousands separators.
	var result []byte
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			result = append(result, '.')
		}
		result = append(result, byte(c))
	}

	formatted := string(result) + "," + decPart + "€"

	if negative {
		formatted = "-" + formatted
	}

	return formatted
}

func totalClass(d decimal.Decimal) string {
	if d.IsPositive() {
		return "total positive"
	}
	if d.IsNegative() {
		return "total negative"
	}
	return "total"
}
