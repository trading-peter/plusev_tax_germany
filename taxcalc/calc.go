package taxcalc

import (
	"time"

	"github.com/plusev-terminal/go-plugin-common/tax"
	"github.com/shopspring/decimal"
)

const (
	// Fallback holding-period threshold used only when timestamps are missing
	// or cannot be parsed. The primary rule uses exact calendar-year comparison.
	LongTermDays = 365

	CatPrivateVeraeusserung = "private_veraeusserung" // §23 EStG — private sale
	CatExemptLongTerm       = "exempt_long_term"      // §23 Abs. 1 Nr. 2 — held > 1 year
	CatExemptFreigrenze     = "exempt_freigrenze"     // §23 Abs. 3 Satz 5 — under Freigrenze
	CatSonstigeEinkuenfte   = "sonstige_einkuenfte"   // §22 Nr. 3 — staking, airdrops, mining
	CatKapitalertraege      = "kapitalertraege"       // §20 EStG — margin, derivatives (Abgeltungsteuer)
	CatTransferFee          = "transfer_fee"          // Fee disposed during transfer
)

// SaleResult holds the computed PnL and metadata for a FIFO-based sale.
type SaleResult struct {
	PnL            decimal.Decimal
	TotalCostBasis decimal.Decimal
	TotalFees      decimal.Decimal
	Proceeds       decimal.Decimal
	Amount         decimal.Decimal
	IsShort        bool
	HoldingDays    int
}

// SplitSaleResult holds split PnL results for short-term and long-term lots.
type SplitSaleResult struct {
	Short *SaleResult
	Long  *SaleResult
}

// ComputeSalePnLAt computes PnL for a SELL trade from FIFO disposal matches.
// If disposalTs and match.LotTs are available, long-term status is determined
// by exact calendar-year comparison instead of a raw day-count approximation.
func ComputeSalePnLAt(proceeds decimal.Decimal, matches []tax.FifoLotMatch, disposalFee decimal.Decimal, disposalTs string) SplitSaleResult {
	var shortMatches, longMatches []tax.FifoLotMatch
	shortAmount := decimal.Zero
	longAmount := decimal.Zero

	for _, m := range matches {
		amt, _ := decimal.NewFromString(m.Amount)

		if isLongTermMatch(m, disposalTs) {
			longMatches = append(longMatches, m)
			longAmount = longAmount.Add(amt)
		} else {
			shortMatches = append(shortMatches, m)
			shortAmount = shortAmount.Add(amt)
		}
	}

	totalAmount := shortAmount.Add(longAmount)

	var result SplitSaleResult

	if len(shortMatches) > 0 {
		result.Short = computeBucketPnL(shortMatches, shortAmount, totalAmount, proceeds, disposalFee, true)
	}

	if len(longMatches) > 0 {
		result.Long = computeBucketPnL(longMatches, longAmount, totalAmount, proceeds, disposalFee, false)
	}

	return result
}

func computeBucketPnL(matches []tax.FifoLotMatch, bucketAmount, totalAmount, totalProceeds, totalDisposalFee decimal.Decimal, isShort bool) *SaleResult {
	totalCostBasis := decimal.Zero
	totalFees := decimal.Zero
	holdingDays := 0

	for _, m := range matches {
		cb, _ := decimal.NewFromString(m.CostBasis)
		fee, _ := decimal.NewFromString(m.Fee)
		totalCostBasis = totalCostBasis.Add(cb)
		totalFees = totalFees.Add(fee)

		// Use the first lot's holding days as representative for the bucket.
		// IsShort is already correct per-bucket, so this is only informational.
		if holdingDays == 0 {
			holdingDays = m.HoldingDays
		}
	}

	// Allocate proceeds and disposal fee proportionally by amount.
	var proceeds, disposalFee decimal.Decimal

	if totalAmount.IsPositive() {
		ratio := bucketAmount.Div(totalAmount)
		proceeds = totalProceeds.Mul(ratio)
		disposalFee = totalDisposalFee.Mul(ratio)
	}

	pnl := proceeds.Sub(totalCostBasis).Sub(totalFees).Sub(disposalFee)

	return &SaleResult{
		PnL:            pnl,
		TotalCostBasis: totalCostBasis,
		TotalFees:      totalFees,
		Proceeds:       proceeds,
		Amount:         bucketAmount,
		IsShort:        isShort,
		HoldingDays:    holdingDays,
	}
}

// FeePnLResult holds the computed PnL for a transfer fee disposal.
type FeePnLResult struct {
	PnL            decimal.Decimal
	TotalCostBasis decimal.Decimal
	TotalFees      decimal.Decimal
	Amount         decimal.Decimal
	Proceeds       decimal.Decimal
	IsShort        bool
	HoldingDays    int
}

// SplitFeePnLResult holds split PnL results for short-term and long-term fee disposals.
type SplitFeePnLResult struct {
	Short *FeePnLResult
	Long  *FeePnLResult
}

// ComputeFeePnLAt computes PnL for a transfer fee disposal.
// If disposalTs and match.LotTs are available, long-term status is determined
// by exact calendar-year comparison instead of a raw day-count approximation.
func ComputeFeePnLAt(feeValueC decimal.Decimal, matches []tax.FifoLotMatch, disposalTs string) SplitFeePnLResult {
	var shortMatches, longMatches []tax.FifoLotMatch
	shortAmount := decimal.Zero
	longAmount := decimal.Zero

	for _, m := range matches {
		amt, _ := decimal.NewFromString(m.Amount)

		if isLongTermMatch(m, disposalTs) {
			longMatches = append(longMatches, m)
			longAmount = longAmount.Add(amt)
		} else {
			shortMatches = append(shortMatches, m)
			shortAmount = shortAmount.Add(amt)
		}
	}

	totalAmount := shortAmount.Add(longAmount)

	var result SplitFeePnLResult

	if len(shortMatches) > 0 {
		result.Short = computeFeeBucket(shortMatches, shortAmount, totalAmount, feeValueC, true)
	}

	if len(longMatches) > 0 {
		result.Long = computeFeeBucket(longMatches, longAmount, totalAmount, feeValueC, false)
	}

	return result
}

func computeFeeBucket(matches []tax.FifoLotMatch, bucketAmount, totalAmount, totalFeeValueC decimal.Decimal, isShort bool) *FeePnLResult {
	totalCostBasis := decimal.Zero
	totalFees := decimal.Zero
	holdingDays := 0

	for _, m := range matches {
		cb, _ := decimal.NewFromString(m.CostBasis)
		fee, _ := decimal.NewFromString(m.Fee)
		totalCostBasis = totalCostBasis.Add(cb)
		totalFees = totalFees.Add(fee)

		// Use the first lot's holding days as representative for the bucket.
		if holdingDays == 0 {
			holdingDays = m.HoldingDays
		}
	}

	// Allocate feeValueC proportionally by amount.
	var allocatedFeeValue decimal.Decimal

	if totalAmount.IsPositive() {
		ratio := bucketAmount.Div(totalAmount)
		allocatedFeeValue = totalFeeValueC.Mul(ratio)
	}

	pnl := allocatedFeeValue.Sub(totalCostBasis).Sub(totalFees)

	return &FeePnLResult{
		PnL:            pnl,
		TotalCostBasis: totalCostBasis,
		TotalFees:      totalFees,
		Amount:         bucketAmount,
		Proceeds:       allocatedFeeValue,
		IsShort:        isShort,
		HoldingDays:    holdingDays,
	}
}

// ComputeMarginPnL computes PnL for margin/derivative trades (no FIFO).
func ComputeMarginPnL(valueC, feeC decimal.Decimal) decimal.Decimal {
	return valueC.Sub(feeC)
}

// DerivativeCloseResult holds the computed PnL and report fields for closing
// a derivative position (long or short).
type DerivativeCloseResult struct {
	PnL             decimal.Decimal
	ReportCostBasis decimal.Decimal // For the report entry CostBasis field.
	ReportProceeds  decimal.Decimal // For the report entry Proceeds field.
}

// ComputeDerivativeLongClosePnL computes PnL for closing a long derivative position.
//
// closeProceeds: allocated sell proceeds for the matched portion.
// matches:       FIFO lot matches from the long position.
// closeFee:      allocated closing-side fee.
//
// PnL = closeProceeds - totalCostBasis - totalLotFees - closeFee.
// ReportCostBasis includes all costs (basis + open fees + close fee).
// ReportProceeds = closeProceeds.
func ComputeDerivativeLongClosePnL(closeProceeds decimal.Decimal, matches []tax.FifoLotMatch, closeFee decimal.Decimal) DerivativeCloseResult {
	totalCostBasis := decimal.Zero
	totalLotFees := decimal.Zero
	for _, m := range matches {
		cb, _ := decimal.NewFromString(m.CostBasis)
		fee, _ := decimal.NewFromString(m.Fee)
		totalCostBasis = totalCostBasis.Add(cb)
		totalLotFees = totalLotFees.Add(fee)
	}

	pnl := closeProceeds.Sub(totalCostBasis).Sub(totalLotFees).Sub(closeFee)

	return DerivativeCloseResult{
		PnL:             pnl,
		ReportCostBasis: totalCostBasis.Add(totalLotFees).Add(closeFee),
		ReportProceeds:  closeProceeds,
	}
}

// ComputeDerivativeShortClosePnL computes PnL for closing a short derivative position.
//
// buyBackCost:   total cost of buying back the matched amount.
// matches:       FIFO lot matches from the SHORT position (CostBasis = original sell proceeds).
// closeFee:      allocated closing-side fee.
//
// PnL = totalShortProceeds - buyBackCost - totalOpenFees - closeFee.
// ReportProceeds = totalShortProceeds - totalOpenFees (net received on open).
// ReportCostBasis = buyBackCost + closeFee (total cost to close).
func ComputeDerivativeShortClosePnL(buyBackCost decimal.Decimal, matches []tax.FifoLotMatch, closeFee decimal.Decimal) DerivativeCloseResult {
	totalShortProceeds := decimal.Zero
	totalOpenFees := decimal.Zero
	for _, m := range matches {
		cb, _ := decimal.NewFromString(m.CostBasis)
		fee, _ := decimal.NewFromString(m.Fee)
		totalShortProceeds = totalShortProceeds.Add(cb)
		totalOpenFees = totalOpenFees.Add(fee)
	}

	pnl := totalShortProceeds.Sub(buyBackCost).Sub(totalOpenFees).Sub(closeFee)

	return DerivativeCloseResult{
		PnL:             pnl,
		ReportCostBasis: buyBackCost.Add(closeFee),
		ReportProceeds:  totalShortProceeds.Sub(totalOpenFees),
	}
}

// AllocateFee allocates total fee proportionally to the matched portion.
// If the entire amount is matched (unmatched == 0), returns the full fee.
func AllocateFee(totalFee, matchedAmount, totalAmount decimal.Decimal) decimal.Decimal {
	if !matchedAmount.IsPositive() || !totalAmount.IsPositive() {
		return decimal.Zero
	}

	if matchedAmount.Equal(totalAmount) {
		return totalFee
	}

	return totalFee.Mul(matchedAmount).Div(totalAmount)
}

// AllocateProceeds allocates total proceeds proportionally to the matched portion.
// If the entire amount is matched, returns the full proceeds.
func AllocateProceeds(totalProceeds, matchedAmount, totalAmount decimal.Decimal) decimal.Decimal {
	if !matchedAmount.IsPositive() || !totalAmount.IsPositive() {
		return decimal.Zero
	}

	if matchedAmount.Equal(totalAmount) {
		return totalProceeds
	}

	return totalProceeds.Mul(matchedAmount).Div(totalAmount)
}

// ComputeShortClosePnLAt computes PnL for closing a margin-spot short position.
// Matches come from the ASSET:SHORT FIFO pool, where CostBasis = original sell
// proceeds and Fee = opening-side fee (both per-unit, multiplied out by the engine).
//
// Unlike derivative short closes, this version splits matches by holding period
// (§ 23 EStG) and returns a SplitSaleResult for Freigrenze participation.
//
// PnL per bucket = shortOpenProceeds - allocatedBuyBackCost - openFees - closeFee.
func ComputeShortClosePnLAt(buyBackCost decimal.Decimal, matches []tax.FifoLotMatch, closeFee decimal.Decimal, disposalTs string) SplitSaleResult {
	var shortMatches, longMatches []tax.FifoLotMatch
	shortAmount := decimal.Zero
	longAmount := decimal.Zero

	for _, m := range matches {
		amt, _ := decimal.NewFromString(m.Amount)

		if isLongTermMatch(m, disposalTs) {
			longMatches = append(longMatches, m)
			longAmount = longAmount.Add(amt)
		} else {
			shortMatches = append(shortMatches, m)
			shortAmount = shortAmount.Add(amt)
		}
	}

	totalAmount := shortAmount.Add(longAmount)

	var result SplitSaleResult

	if len(shortMatches) > 0 {
		result.Short = computeShortCloseBucketPnL(shortMatches, shortAmount, totalAmount, buyBackCost, closeFee, true)
	}

	if len(longMatches) > 0 {
		result.Long = computeShortCloseBucketPnL(longMatches, longAmount, totalAmount, buyBackCost, closeFee, false)
	}

	return result
}

func computeShortCloseBucketPnL(matches []tax.FifoLotMatch, bucketAmount, totalAmount, totalBuyBackCost, totalCloseFee decimal.Decimal, isShort bool) *SaleResult {
	totalShortProceeds := decimal.Zero
	totalOpenFees := decimal.Zero
	holdingDays := 0

	for _, m := range matches {
		cb, _ := decimal.NewFromString(m.CostBasis)
		fee, _ := decimal.NewFromString(m.Fee)
		totalShortProceeds = totalShortProceeds.Add(cb)
		totalOpenFees = totalOpenFees.Add(fee)

		if holdingDays == 0 {
			holdingDays = m.HoldingDays
		}
	}

	// Allocate buy-back cost and close fee proportionally by amount.
	var buyBackCost, closeFee decimal.Decimal

	if totalAmount.IsPositive() {
		ratio := bucketAmount.Div(totalAmount)
		buyBackCost = totalBuyBackCost.Mul(ratio)
		closeFee = totalCloseFee.Mul(ratio)
	}

	// For margin-spot shorts:
	// Proceeds = net short-open proceeds (what we received minus opening fees)
	// CostBasis = buy-back cost (what we paid to close), TotalFees = closeFee
	// PnL = totalShortProceeds - buyBackCost - openFees - closeFee
	proceeds := totalShortProceeds.Sub(totalOpenFees)
	pnl := totalShortProceeds.Sub(buyBackCost).Sub(totalOpenFees).Sub(closeFee)

	return &SaleResult{
		PnL:            pnl,
		TotalCostBasis: buyBackCost,
		TotalFees:      closeFee,
		Proceeds:       proceeds,
		Amount:         bucketAmount,
		IsShort:        isShort,
		HoldingDays:    holdingDays,
	}
}

// ComputeProceeds returns the sale proceeds, falling back to amount*price if valueC is zero.
func ComputeProceeds(valueC, amount, priceC decimal.Decimal) decimal.Decimal {
	if !valueC.IsZero() {
		return valueC
	}

	return amount.Mul(priceC)
}

// ComputePerUnitCost returns per-unit cost basis, falling back to priceC if valueC is zero.
func ComputePerUnitCost(valueC, amount, priceC decimal.Decimal) decimal.Decimal {
	if amount.GreaterThan(decimal.Zero) && !valueC.IsZero() {
		return valueC.Div(amount)
	}

	return priceC
}

// ComputePerUnitFee returns per-unit fee, or zero if amount or feeC is zero.
func ComputePerUnitFee(feeC, amount decimal.Decimal) decimal.Decimal {
	if amount.GreaterThan(decimal.Zero) && !feeC.IsZero() {
		return feeC.Div(amount)
	}

	return decimal.Zero
}

func isLongTermMatch(match tax.FifoLotMatch, disposalTs string) bool {
	return isLongTermHolding(match.LotTs, disposalTs, match.HoldingDays)
}

// isLongTermHolding applies the exact calendar-year holding rule when both
// timestamps are available, and falls back to HoldingDays otherwise.
func isLongTermHolding(lotTs, disposalTs string, holdingDays int) bool {
	if lotTs == "" || disposalTs == "" {
		return holdingDays > LongTermDays
	}

	acquiredDate, ok := parseCivilDate(lotTs)
	if !ok {
		return holdingDays > LongTermDays
	}

	disposalDate, ok := parseCivilDate(disposalTs)
	if !ok {
		return holdingDays > LongTermDays
	}

	// German §23 uses a one-year holding period. For exact calendar handling,
	// treat Feb 29 -> Feb 28 in the following year as the one-year boundary,
	// and require the disposal date to be after that boundary to be tax-exempt.
	return disposalDate.After(addOneCalendarYearClamped(acquiredDate))
}

func parseCivilDate(ts string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false
	}

	year, month, day := parsed.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC), true
}

func addOneCalendarYearClamped(date time.Time) time.Time {
	year, month, day := date.Date()
	nextYear := year + 1
	lastDay := daysInMonth(nextYear, month)
	if day > lastDay {
		day = lastDay
	}

	return time.Date(nextYear, month, day, 0, 0, 0, 0, time.UTC)
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// DetermineSaleCategory returns the tax category for a sale based on holding period.
func DetermineSaleCategory(isShort bool) string {
	if isShort {
		return CatPrivateVeraeusserung
	}

	return CatExemptLongTerm
}

// DetermineTransferFeeCategory returns the tax category for a transfer fee disposal.
func DetermineTransferFeeCategory(isShort bool) string {
	if isShort {
		return CatTransferFee
	}

	return CatExemptLongTerm
}

// RecordEntry mirrors the internal recordEntry from the plugin for Freigrenze computation.
// The metadata fields (Account, Ticker, etc.) are populated by the main processing loop
// after processRecord returns — they are used for document generation, not tax logic.
type RecordEntry struct {
	Entry   tax.PluginReportEntry
	PnL     decimal.Decimal
	IsShort bool

	// Document generation metadata (set in main loop from the source PluginRecord).
	Account          string
	Ticker           string
	Quote            string
	Action           int  // 0=buy/deposit, 1=sell/withdrawal
	IsDerivative     bool // true for derivative and margin-spot trades
	TransferCategory string
	FeeC             decimal.Decimal // disposal-side fee in cost-basis currency
}

// FreigrenzeForYear returns the applicable Freigrenze for the given tax year.
// Before 2024: 600€ (§23 Abs. 3 Satz 5 EStG).
// From 2024: 1000€ (Wachstumschancengesetz).
func FreigrenzeForYear(taxYear int) decimal.Decimal {
	if taxYear >= 2024 {
		return decimal.NewFromInt(1000)
	}

	return decimal.NewFromInt(600)
}

// FreigrenzeResult holds the result of the Freigrenze computation.
type FreigrenzeResult struct {
	NetShortTermPnL decimal.Decimal
	ShouldApply     bool
}

// ComputeFreigrenze checks whether the Freigrenze applies for the given tax year.
// Nets ALL short-term §23 PnLs (gains and losses) to compute the Gesamtgewinn.
// Only applies if 0 < net <= threshold. If net <= 0 (loss year), entries must
// remain fully declared for Verlustvortrag (loss carry-forward).
func ComputeFreigrenze(entries []RecordEntry, taxYear int) FreigrenzeResult {
	netShortTermPnL := decimal.Zero

	for _, e := range entries {
		if !e.IsShort {
			continue
		}

		cat := e.Entry.TaxCategory
		if cat == CatPrivateVeraeusserung || cat == CatTransferFee {
			netShortTermPnL = netShortTermPnL.Add(e.PnL)
		}
	}

	threshold := FreigrenzeForYear(taxYear)
	shouldApply := netShortTermPnL.IsPositive() && netShortTermPnL.LessThanOrEqual(threshold)

	return FreigrenzeResult{
		NetShortTermPnL: netShortTermPnL,
		ShouldApply:     shouldApply,
	}
}

// ApplyFreigrenze modifies entries in-place: if Freigrenze applies,
// short-term private_veraeusserung and transfer_fee entries are
// re-categorized as exempt_freigrenze with taxableAmount = "0".
func ApplyFreigrenze(entries []RecordEntry, taxYear int) FreigrenzeResult {
	result := ComputeFreigrenze(entries, taxYear)

	if !result.ShouldApply {
		return result
	}

	for i := range entries {
		if !entries[i].IsShort {
			continue
		}

		cat := entries[i].Entry.TaxCategory
		if cat == CatPrivateVeraeusserung || cat == CatTransferFee {
			entries[i].Entry.TaxCategory = CatExemptFreigrenze
			entries[i].Entry.TaxableAmount = "0"
		}
	}

	return result
}
