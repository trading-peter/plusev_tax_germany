package taxcalc

import (
	"testing"
	"time"

	"github.com/plusev-terminal/go-plugin-common/tax"
	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal {
	v, _ := decimal.NewFromString(s)
	return v
}

const testDisposalTs = "2025-12-31T00:00:00Z"

func computeSalePnLForTest(proceeds decimal.Decimal, matches []tax.FifoLotMatch, disposalFee decimal.Decimal) SplitSaleResult {
	return ComputeSalePnLAt(proceeds, stampMatchesWithLotTs(matches, testDisposalTs), disposalFee, testDisposalTs)
}

func computeFeePnLForTest(feeValueC decimal.Decimal, matches []tax.FifoLotMatch) SplitFeePnLResult {
	return ComputeFeePnLAt(feeValueC, stampMatchesWithLotTs(matches, testDisposalTs), testDisposalTs)
}

func stampMatchesWithLotTs(matches []tax.FifoLotMatch, disposalTs string) []tax.FifoLotMatch {
	disposalTime, err := time.Parse(time.RFC3339, disposalTs)
	if err != nil {
		panic(err)
	}

	stamped := make([]tax.FifoLotMatch, len(matches))
	copy(stamped, matches)

	for i := range stamped {
		if stamped[i].LotTs != "" {
			continue
		}

		lotTime := disposalTime.AddDate(0, 0, -stamped[i].HoldingDays)
		stamped[i].LotTs = lotTime.Format(time.RFC3339)
	}

	return stamped
}

// --- ComputeSalePnLAt ---

func TestComputeSalePnL_BasicProfit(t *testing.T) {
	// Buy 10 BTC at 100€ each (cost=1000€), sell at 150€ each (proceeds=1500€), fee=5€
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "10", CostBasis: "1000", Fee: "50", HoldingDays: 30},
	}

	result := computeSalePnLForTest(d("1500"), matches, d("5"))

	// All short-term, so only Short bucket.
	// PnL = 1500 - 1000 - 50 - 5 = 445
	assertNotNil(t, "Short", result.Short)
	assertNil(t, "Long", result.Long)
	assertDecimal(t, "PnL", d("445"), result.Short.PnL)
	assertDecimal(t, "TotalCostBasis", d("1000"), result.Short.TotalCostBasis)
	assertDecimal(t, "TotalFees", d("50"), result.Short.TotalFees)
	assertTrue(t, "IsShort", result.Short.IsShort)
	assertEqual(t, "HoldingDays", 30, result.Short.HoldingDays)
}

func TestComputeSalePnL_BasicLoss(t *testing.T) {
	// Buy at 200€, sell at 100€
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "200", Fee: "10", HoldingDays: 100},
	}

	result := computeSalePnLForTest(d("100"), matches, d("5"))

	// PnL = 100 - 200 - 10 - 5 = -115
	assertNotNil(t, "Short", result.Short)
	assertDecimal(t, "PnL", d("-115"), result.Short.PnL)
	assertTrue(t, "IsShort", result.Short.IsShort)
}

func TestComputeSalePnL_AllLongTerm(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "10", CostBasis: "1000", Fee: "50", HoldingDays: 400},
		{LotID: "lot2", Amount: "5", CostBasis: "600", Fee: "30", HoldingDays: 500},
	}

	result := computeSalePnLForTest(d("2000"), matches, d("0"))

	assertNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)
	assertFalse(t, "Long.IsShort", result.Long.IsShort)
}

func TestComputeSalePnL_ExactlyAtBoundary365(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "100", Fee: "0", HoldingDays: 365},
	}

	result := computeSalePnLForTest(d("200"), matches, d("0"))

	assertNotNil(t, "Short", result.Short)
	assertNil(t, "Long", result.Long)
	assertTrue(t, "365 days is short-term", result.Short.IsShort)
}

func TestComputeSalePnL_At366DaysIsLongTerm(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "100", Fee: "0", HoldingDays: 366},
	}

	result := computeSalePnLForTest(d("200"), matches, d("0"))

	assertNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)
	assertFalse(t, "366 days is long-term", result.Long.IsShort)
}

func TestHoldingPeriod_LeapYearBoundaryCases(t *testing.T) {
	tests := []struct {
		name       string
		acquired   string
		disposed   string
		wantIsLong bool
	}{
		{
			name:       "leap_day_to_feb_28_next_year",
			acquired:   "2024-02-29",
			disposed:   "2025-02-28",
			wantIsLong: false,
		},
		{
			name:       "leap_day_to_march_1_next_year",
			acquired:   "2024-02-29",
			disposed:   "2025-03-01",
			wantIsLong: true,
		},
		{
			name:       "same_calendar_date_next_year_is_still_short_term",
			acquired:   "2024-01-15",
			disposed:   "2025-01-15",
			wantIsLong: false,
		},
		{
			name:       "day_after_same_calendar_date_next_year_is_long_term",
			acquired:   "2024-01-15",
			disposed:   "2025-01-16",
			wantIsLong: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acquired, err := time.Parse("2006-01-02", tt.acquired)
			if err != nil {
				t.Fatalf("parse acquired: %v", err)
			}

			disposed, err := time.Parse("2006-01-02", tt.disposed)
			if err != nil {
				t.Fatalf("parse disposed: %v", err)
			}

			holdingDays := int(disposed.Sub(acquired).Hours() / 24)
			gotIsLong := isLongTermHolding(tt.acquired+"T00:00:00Z", tt.disposed+"T00:00:00Z", holdingDays)

			if gotIsLong != tt.wantIsLong {
				t.Fatalf("unexpected long-term result: holdingDays=%d wantIsLong=%v got=%v", holdingDays, tt.wantIsLong, gotIsLong)
			}
		})
	}
}

func TestComputeSalePnLAt_UsesCalendarYearBoundary(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{
			LotID:       "lot1",
			LotTs:       "2024-02-29T00:00:00Z",
			Amount:      "1",
			CostBasis:   "100",
			Fee:         "0",
			HoldingDays: 365,
		},
	}

	result := ComputeSalePnLAt(d("200"), matches, d("0"), "2025-03-01T00:00:00Z")

	assertNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)
	assertFalse(t, "Calendar-year logic should override raw day count", result.Long.IsShort)
}

func TestComputeFeePnLAt_UsesCalendarYearBoundary(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{
			LotID:       "lot1",
			LotTs:       "2024-02-29T00:00:00Z",
			Amount:      "0.1",
			CostBasis:   "5",
			Fee:         "0",
			HoldingDays: 365,
		},
	}

	result := ComputeFeePnLAt(d("10"), matches, "2025-03-01T00:00:00Z")

	assertNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)
	assertFalse(t, "Calendar-year logic should override raw day count", result.Long.IsShort)
}

func TestComputeSalePnL_NoMatches(t *testing.T) {
	result := computeSalePnLForTest(d("500"), nil, d("10"))

	// No matches = no buckets
	assertNil(t, "Short", result.Short)
	assertNil(t, "Long", result.Long)
}

func TestComputeSalePnL_MixedHolding_SplitCorrectly(t *testing.T) {
	// 5 BTC short-term (cost 500€, fee 25€) + 5 BTC long-term (cost 1000€, fee 50€)
	// Total amount = 10, proceeds = 3000€, disposal fee = 10€
	// Short ratio = 5/10 = 0.5, Long ratio = 5/10 = 0.5
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "5", CostBasis: "500", Fee: "25", HoldingDays: 100},
		{LotID: "lot2", Amount: "5", CostBasis: "1000", Fee: "50", HoldingDays: 400},
	}

	result := computeSalePnLForTest(d("3000"), matches, d("10"))

	assertNotNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)

	// Short: proceeds=1500, cost=500, fees=25, disposalFee=5 → PnL=970
	assertDecimal(t, "Short.PnL", d("970"), result.Short.PnL)
	assertDecimal(t, "Short.CostBasis", d("500"), result.Short.TotalCostBasis)
	assertDecimal(t, "Short.Fees", d("25"), result.Short.TotalFees)
	assertTrue(t, "Short.IsShort", result.Short.IsShort)

	// Long: proceeds=1500, cost=1000, fees=50, disposalFee=5 → PnL=445
	assertDecimal(t, "Long.PnL", d("445"), result.Long.PnL)
	assertDecimal(t, "Long.CostBasis", d("1000"), result.Long.TotalCostBasis)
	assertDecimal(t, "Long.Fees", d("50"), result.Long.TotalFees)
	assertFalse(t, "Long.IsShort", result.Long.IsShort)

	// Total PnL should equal the old unsplit total: 970 + 445 = 1415
	totalPnL := result.Short.PnL.Add(result.Long.PnL)
	assertDecimal(t, "TotalPnL", d("1415"), totalPnL)
}

func TestComputeSalePnL_MixedHolding_UnequalAmounts(t *testing.T) {
	// 2 BTC short-term + 8 BTC long-term, proceeds = 1000€, fee = 10€
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "2", CostBasis: "100", Fee: "5", HoldingDays: 30},
		{LotID: "lot2", Amount: "8", CostBasis: "400", Fee: "20", HoldingDays: 500},
	}

	result := computeSalePnLForTest(d("1000"), matches, d("10"))

	// Short ratio = 2/10 = 0.2 → proceeds=200, fee=2
	// Short PnL = 200 - 100 - 5 - 2 = 93
	assertDecimal(t, "Short.PnL", d("93"), result.Short.PnL)

	// Long ratio = 8/10 = 0.8 → proceeds=800, fee=8
	// Long PnL = 800 - 400 - 20 - 8 = 372
	assertDecimal(t, "Long.PnL", d("372"), result.Long.PnL)
}

func TestComputeSalePnL_MixedHolding_ZeroDisposalFee(t *testing.T) {
	// Zero disposal fee should not break ratio allocation.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "3", CostBasis: "300", Fee: "15", HoldingDays: 60},
		{LotID: "lot2", Amount: "7", CostBasis: "700", Fee: "35", HoldingDays: 400},
	}

	result := computeSalePnLForTest(d("2000"), matches, d("0"))

	assertNotNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)

	// Short ratio = 3/10 = 0.3 → proceeds=600, fee=0
	// Short PnL = 600 - 300 - 15 - 0 = 285
	assertDecimal(t, "Short.PnL", d("285"), result.Short.PnL)

	// Long ratio = 7/10 = 0.7 → proceeds=1400, fee=0
	// Long PnL = 1400 - 700 - 35 - 0 = 665
	assertDecimal(t, "Long.PnL", d("665"), result.Long.PnL)

	// Total preserved: 285 + 665 = 950
	totalPnL := result.Short.PnL.Add(result.Long.PnL)
	assertDecimal(t, "TotalPnL", d("950"), totalPnL)
}

func TestComputeSalePnL_MixedHolding_TinyFractionalAmounts(t *testing.T) {
	// Tiny short-term + large long-term to test decimal precision in ratios.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "0.00012345", CostBasis: "5", Fee: "0.01", HoldingDays: 30},
		{LotID: "lot2", Amount: "9.99987655", CostBasis: "50000", Fee: "25", HoldingDays: 500},
	}

	result := computeSalePnLForTest(d("60000"), matches, d("10"))

	assertNotNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)

	// Verify total PnL is preserved (no precision loss).
	// Total: 60000 - (5 + 50000) - (0.01 + 25) - 10 = 9959.99
	totalPnL := result.Short.PnL.Add(result.Long.PnL)
	assertDecimal(t, "TotalPnL", d("9959.99"), totalPnL)

	// Short ratio is tiny (~0.0000123), long ratio is ~0.9999877.
	// Short bucket should have very small proceeds.
	assertTrue(t, "Short PnL is small", result.Short.PnL.Abs().LessThan(d("10")))
	assertTrue(t, "Long PnL is large", result.Long.PnL.GreaterThan(d("9950")))
}

func TestComputeSalePnL_NegativeProceeds(t *testing.T) {
	// Negative proceeds — unlikely in practice, but proves PnL handles it.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "100", Fee: "5", HoldingDays: 30},
	}

	result := computeSalePnLForTest(d("-50"), matches, d("10"))

	assertNotNil(t, "Short", result.Short)
	// PnL = -50 - 100 - 5 - 10 = -165
	assertDecimal(t, "PnL", d("-165"), result.Short.PnL)
}

func TestComputeSalePnL_ProportionalAmountAndProceeds(t *testing.T) {
	// Verify that split sale rows carry proportional Amount and Proceeds.
	// 3 BTC short + 7 BTC long, proceeds = 2000€, fee = 10€
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "3", CostBasis: "300", Fee: "15", HoldingDays: 60},
		{LotID: "lot2", Amount: "7", CostBasis: "700", Fee: "35", HoldingDays: 400},
	}

	result := computeSalePnLForTest(d("2000"), matches, d("10"))

	assertDecimal(t, "Short.Amount", d("3"), result.Short.Amount)
	assertDecimal(t, "Short.Proceeds", d("600"), result.Short.Proceeds)

	assertDecimal(t, "Long.Amount", d("7"), result.Long.Amount)
	assertDecimal(t, "Long.Proceeds", d("1400"), result.Long.Proceeds)
}

// --- ComputeFeePnLAt ---

func TestComputeFeePnL_BasicProfit(t *testing.T) {
	// Fee worth 10€ in EUR, lots acquired at 8€ with no acquisition fee.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "0.001", CostBasis: "8", Fee: "0", HoldingDays: 30},
	}

	result := computeFeePnLForTest(d("10"), matches)

	assertNotNil(t, "Short", result.Short)
	assertNil(t, "Long", result.Long)
	assertDecimal(t, "PnL", d("2"), result.Short.PnL)
	assertTrue(t, "IsShort", result.Short.IsShort)
}

func TestComputeFeePnL_LongTermFee(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "0.001", CostBasis: "5", Fee: "0", HoldingDays: 700},
	}

	result := computeFeePnLForTest(d("10"), matches)

	assertNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)
	assertDecimal(t, "PnL", d("5"), result.Long.PnL)
	assertFalse(t, "IsShort", result.Long.IsShort)
}

func TestComputeFeePnL_WithAcquisitionFees(t *testing.T) {
	// Fee worth 10€, lot cost 5€ with 1€ acquisition fee. PnL = 10 - 5 - 1 = 4.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "0.001", CostBasis: "5", Fee: "1", HoldingDays: 30},
	}

	result := computeFeePnLForTest(d("10"), matches)

	assertNotNil(t, "Short", result.Short)
	assertDecimal(t, "PnL", d("4"), result.Short.PnL)
	assertDecimal(t, "TotalCostBasis", d("5"), result.Short.TotalCostBasis)
	assertDecimal(t, "TotalFees", d("1"), result.Short.TotalFees)
}

func TestComputeFeePnL_MixedHoldingPeriod(t *testing.T) {
	// 0.0005 BTC short-term (cost 3€, fee 0.5€) + 0.0005 BTC long-term (cost 4€, fee 0.5€)
	// Total fee value = 10€. Each bucket gets 50% = 5€.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "0.0005", CostBasis: "3", Fee: "0.5", HoldingDays: 30},
		{LotID: "lot2", Amount: "0.0005", CostBasis: "4", Fee: "0.5", HoldingDays: 400},
	}

	result := computeFeePnLForTest(d("10"), matches)

	assertNotNil(t, "Short", result.Short)
	assertNotNil(t, "Long", result.Long)

	// Short: proceeds=5, cost=3, fee=0.5 → PnL=1.5
	assertDecimal(t, "Short.PnL", d("1.5"), result.Short.PnL)
	assertTrue(t, "Short.IsShort", result.Short.IsShort)

	// Long: proceeds=5, cost=4, fee=0.5 → PnL=0.5 (exempt)
	assertDecimal(t, "Long.PnL", d("0.5"), result.Long.PnL)
	assertFalse(t, "Long.IsShort", result.Long.IsShort)

	// Total PnL preserved: 1.5 + 0.5 = 2
	totalPnL := result.Short.PnL.Add(result.Long.PnL)
	assertDecimal(t, "TotalPnL", d("2"), totalPnL)
}

func TestComputeFeePnL_NoMatches(t *testing.T) {
	result := computeFeePnLForTest(d("10"), nil)

	assertNil(t, "Short", result.Short)
	assertNil(t, "Long", result.Long)
}

func TestComputeFeePnL_ProportionalAmountAndProceeds(t *testing.T) {
	// 0.001 short + 0.003 long = total 0.004, fee value = 20€
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "0.001", CostBasis: "2", Fee: "0", HoldingDays: 30},
		{LotID: "lot2", Amount: "0.003", CostBasis: "6", Fee: "0", HoldingDays: 400},
	}

	result := computeFeePnLForTest(d("20"), matches)

	// Short ratio = 0.001/0.004 = 0.25 → proceeds=5, amount=0.001
	assertDecimal(t, "Short.Amount", d("0.001"), result.Short.Amount)
	assertDecimal(t, "Short.Proceeds", d("5"), result.Short.Proceeds)

	// Long ratio = 0.003/0.004 = 0.75 → proceeds=15, amount=0.003
	assertDecimal(t, "Long.Amount", d("0.003"), result.Long.Amount)
	assertDecimal(t, "Long.Proceeds", d("15"), result.Long.Proceeds)
}

// --- ComputeMarginPnL ---

func TestComputeMarginPnL(t *testing.T) {
	tests := []struct {
		name   string
		valueC string
		feeC   string
		want   string
	}{
		{"profit", "1000", "50", "950"},
		{"loss", "-500", "50", "-550"},
		{"zero", "100", "100", "0"},
		{"no_fee", "250", "0", "250"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeMarginPnL(d(tt.valueC), d(tt.feeC))
			assertDecimal(t, "PnL", d(tt.want), got)
		})
	}
}

// --- ComputeProceeds ---

func TestComputeProceeds(t *testing.T) {
	tests := []struct {
		name   string
		valueC string
		amount string
		priceC string
		want   string
	}{
		{"uses_valueC", "1500", "10", "100", "1500"},
		{"fallback_to_amount_times_price", "0", "10", "150", "1500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeProceeds(d(tt.valueC), d(tt.amount), d(tt.priceC))
			assertDecimal(t, "proceeds", d(tt.want), got)
		})
	}
}

// --- ComputePerUnitCost ---

func TestComputePerUnitCost(t *testing.T) {
	tests := []struct {
		name   string
		valueC string
		amount string
		priceC string
		want   string
	}{
		{"normal_division", "1000", "10", "50", "100"},
		{"zero_amount_fallback", "1000", "0", "50", "50"},
		{"zero_valueC_fallback", "0", "10", "50", "50"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputePerUnitCost(d(tt.valueC), d(tt.amount), d(tt.priceC))
			assertDecimal(t, "perUnitCost", d(tt.want), got)
		})
	}
}

// --- ComputePerUnitFee ---

func TestComputePerUnitFee(t *testing.T) {
	tests := []struct {
		name   string
		feeC   string
		amount string
		want   string
	}{
		{"normal_division", "50", "10", "5"},
		{"zero_amount", "50", "0", "0"},
		{"zero_fee", "0", "10", "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputePerUnitFee(d(tt.feeC), d(tt.amount))
			assertDecimal(t, "perUnitFee", d(tt.want), got)
		})
	}
}

// --- DetermineSaleCategory ---

func TestDetermineSaleCategory(t *testing.T) {
	assertEqual(t, "short-term", CatPrivateVeraeusserung, DetermineSaleCategory(true))
	assertEqual(t, "long-term", CatExemptLongTerm, DetermineSaleCategory(false))
}

// --- DetermineTransferFeeCategory ---

func TestDetermineTransferFeeCategory(t *testing.T) {
	assertEqual(t, "short", CatTransferFee, DetermineTransferFeeCategory(true))
	assertEqual(t, "long", CatExemptLongTerm, DetermineTransferFeeCategory(false))
}

// --- FreigrenzeForYear ---

func TestFreigrenzeForYear(t *testing.T) {
	assertDecimal(t, "2023", d("600"), FreigrenzeForYear(2023))
	assertDecimal(t, "2022", d("600"), FreigrenzeForYear(2022))
	assertDecimal(t, "2024", d("1000"), FreigrenzeForYear(2024))
	assertDecimal(t, "2025", d("1000"), FreigrenzeForYear(2025))
	assertDecimal(t, "2026", d("1000"), FreigrenzeForYear(2026))
}

// --- ComputeFreigrenze ---

func TestComputeFreigrenze_NettingGainsAndLosses(t *testing.T) {
	// +800 gain + (-400) loss = net 400 → should apply (400 <= 600)
	entries := []RecordEntry{
		{PnL: d("800"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
		{PnL: d("-400"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result := ComputeFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("400"), result.NetShortTermPnL)
	assertTrue(t, "ShouldApply (net 400 <= 600)", result.ShouldApply)
}

func TestComputeFreigrenze_NetLoss_MustNotApply(t *testing.T) {
	// +200 gain + (-800) loss = net -600 → must NOT apply (preserve for Verlustvortrag)
	entries := []RecordEntry{
		{PnL: d("200"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
		{PnL: d("-800"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result := ComputeFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("-600"), result.NetShortTermPnL)
	assertFalse(t, "ShouldApply (net negative)", result.ShouldApply)
}

func TestComputeFreigrenze_NetZero_MustNotApply(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("500"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
		{PnL: d("-500"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result := ComputeFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("0"), result.NetShortTermPnL)
	assertFalse(t, "ShouldApply (net == 0)", result.ShouldApply)
}

func TestComputeFreigrenze_UnderThreshold(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("200"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
		{PnL: d("300"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result := ComputeFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("500"), result.NetShortTermPnL)
	assertTrue(t, "ShouldApply (500 <= 600)", result.ShouldApply)
}

func TestComputeFreigrenze_ExactlyAtThreshold(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("600"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result := ComputeFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("600"), result.NetShortTermPnL)
	assertTrue(t, "ShouldApply (600 <= 600)", result.ShouldApply)
}

func TestComputeFreigrenze_OverThreshold(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("601"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result := ComputeFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("601"), result.NetShortTermPnL)
	assertFalse(t, "ShouldApply (601 > 600)", result.ShouldApply)
}

func TestComputeFreigrenze_OnlyCountsShortTermParagraph23(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("5000"), IsShort: false, Entry: tax.PluginReportEntry{TaxCategory: CatExemptLongTerm}},
		{PnL: d("100"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
		{PnL: d("200"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatKapitalertraege}}, // excluded from §23
	}

	result := ComputeFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("100"), result.NetShortTermPnL)
	assertTrue(t, "ShouldApply (100 <= 600)", result.ShouldApply)
}

func TestComputeFreigrenze_IncludesTransferFees(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("300"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
		{PnL: d("200"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatTransferFee}},
	}

	result := ComputeFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("500"), result.NetShortTermPnL)
	assertTrue(t, "ShouldApply (500 <= 600)", result.ShouldApply)
}

func TestComputeFreigrenze_NoEntries(t *testing.T) {
	result := ComputeFreigrenze(nil, 2023)

	assertDecimal(t, "NetShortTermPnL", d("0"), result.NetShortTermPnL)
	assertFalse(t, "ShouldApply (net == 0)", result.ShouldApply)
}

func TestComputeFreigrenze_Year2024_HigherThreshold(t *testing.T) {
	// 800€ gains — over 600€ threshold but under 1000€ threshold
	entries := []RecordEntry{
		{PnL: d("800"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result2023 := ComputeFreigrenze(entries, 2023)
	assertFalse(t, "2023: 800 > 600", result2023.ShouldApply)

	result2024 := ComputeFreigrenze(entries, 2024)
	assertTrue(t, "2024: 800 <= 1000", result2024.ShouldApply)
}

func TestComputeFreigrenze_Year2024_ExactlyAtThreshold(t *testing.T) {
	entries1000 := []RecordEntry{
		{PnL: d("1000"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result := ComputeFreigrenze(entries1000, 2024)
	assertTrue(t, "2024: 1000 <= 1000", result.ShouldApply)

	entries1001 := []RecordEntry{
		{PnL: d("1001"), IsShort: true, Entry: tax.PluginReportEntry{TaxCategory: CatPrivateVeraeusserung}},
	}

	result2 := ComputeFreigrenze(entries1001, 2024)
	assertFalse(t, "2024: 1001 > 1000", result2.ShouldApply)
}

// --- ApplyFreigrenze ---

func TestApplyFreigrenze_UnderThreshold_MarksExempt(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("200"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "200",
		}},
		{PnL: d("100"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx2", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "100",
		}},
	}

	ApplyFreigrenze(entries, 2023)

	assertEqual(t, "tx1 category", CatExemptFreigrenze, entries[0].Entry.TaxCategory)
	assertEqual(t, "tx1 taxable", "0", entries[0].Entry.TaxableAmount)
	assertEqual(t, "tx2 category", CatExemptFreigrenze, entries[1].Entry.TaxCategory)
	assertEqual(t, "tx2 taxable", "0", entries[1].Entry.TaxableAmount)
}

func TestApplyFreigrenze_OverThreshold_NoChange(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("700"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "700",
		}},
	}

	ApplyFreigrenze(entries, 2023)

	assertEqual(t, "category unchanged", CatPrivateVeraeusserung, entries[0].Entry.TaxCategory)
	assertEqual(t, "taxable unchanged", "700", entries[0].Entry.TaxableAmount)
}

func TestApplyFreigrenze_DoesNotAffectLongTerm(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("100"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "100",
		}},
		{PnL: d("5000"), IsShort: false, Entry: tax.PluginReportEntry{
			TxID: "tx2", TaxCategory: CatExemptLongTerm, TaxableAmount: "0",
		}},
	}

	ApplyFreigrenze(entries, 2023)

	assertEqual(t, "short-term gets exempt", CatExemptFreigrenze, entries[0].Entry.TaxCategory)
	assertEqual(t, "long-term unchanged", CatExemptLongTerm, entries[1].Entry.TaxCategory)
}

func TestApplyFreigrenze_AlsoExemptsTransferFees(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("200"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "200",
		}},
		{PnL: d("50"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx2", TaxCategory: CatTransferFee, TaxableAmount: "50",
		}},
	}

	ApplyFreigrenze(entries, 2023)

	assertEqual(t, "sale exempt", CatExemptFreigrenze, entries[0].Entry.TaxCategory)
	assertEqual(t, "fee exempt", CatExemptFreigrenze, entries[1].Entry.TaxCategory)
	assertEqual(t, "fee taxable=0", "0", entries[1].Entry.TaxableAmount)
}

func TestApplyFreigrenze_NetLoss_PreservesEntries(t *testing.T) {
	// Net = +200 - 800 = -600 → must NOT apply (Verlustvortrag)
	entries := []RecordEntry{
		{PnL: d("200"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "200",
		}},
		{PnL: d("-800"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx2", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "-800",
		}},
	}

	result := ApplyFreigrenze(entries, 2023)

	assertFalse(t, "ShouldApply", result.ShouldApply)
	assertEqual(t, "tx1 unchanged", CatPrivateVeraeusserung, entries[0].Entry.TaxCategory)
	assertEqual(t, "tx1 taxable unchanged", "200", entries[0].Entry.TaxableAmount)
	assertEqual(t, "tx2 unchanged", CatPrivateVeraeusserung, entries[1].Entry.TaxCategory)
	assertEqual(t, "tx2 taxable unchanged", "-800", entries[1].Entry.TaxableAmount)
}

func TestApplyFreigrenze_DoesNotAffectKapitalertraege(t *testing.T) {
	entries := []RecordEntry{
		{PnL: d("100"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "100",
		}},
		{PnL: d("500"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx2", TaxCategory: CatKapitalertraege, TaxableAmount: "500",
		}},
	}

	ApplyFreigrenze(entries, 2023)

	assertEqual(t, "sale exempt", CatExemptFreigrenze, entries[0].Entry.TaxCategory)
	assertEqual(t, "kapitalertraege unchanged", CatKapitalertraege, entries[1].Entry.TaxCategory)
	assertEqual(t, "kapitalertraege taxable unchanged", "500", entries[1].Entry.TaxableAmount)
}

// --- End-to-end scenarios ---

func TestScenario_FullYearWithFreigrenze(t *testing.T) {
	// 3 short-term trades, net = 150 + 250 + 150 = 550€ (under 600€)
	entries := []RecordEntry{
		{PnL: d("150"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "150",
		}},
		{PnL: d("250"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx2", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "250",
		}},
		{PnL: d("3000"), IsShort: false, Entry: tax.PluginReportEntry{
			TxID: "tx3", TaxCategory: CatExemptLongTerm, TaxableAmount: "0",
		}},
		{PnL: d("150"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx4", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "150",
		}},
	}

	result := ApplyFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("550"), result.NetShortTermPnL)
	assertTrue(t, "Freigrenze applies", result.ShouldApply)

	assertEqual(t, "tx1 exempt", CatExemptFreigrenze, entries[0].Entry.TaxCategory)
	assertEqual(t, "tx1 taxable=0", "0", entries[0].Entry.TaxableAmount)
	assertEqual(t, "tx2 exempt", CatExemptFreigrenze, entries[1].Entry.TaxCategory)
	assertEqual(t, "tx3 still long-term", CatExemptLongTerm, entries[2].Entry.TaxCategory)
	assertEqual(t, "tx4 exempt", CatExemptFreigrenze, entries[3].Entry.TaxCategory)
}

func TestScenario_FullYearOverFreigrenze(t *testing.T) {
	// Net short-term = 650€ (over 600€ for 2023)
	entries := []RecordEntry{
		{PnL: d("300"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "300",
		}},
		{PnL: d("350"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx2", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "350",
		}},
	}

	result := ApplyFreigrenze(entries, 2023)

	assertDecimal(t, "NetShortTermPnL", d("650"), result.NetShortTermPnL)
	assertFalse(t, "Freigrenze does NOT apply", result.ShouldApply)

	assertEqual(t, "tx1 still taxable", CatPrivateVeraeusserung, entries[0].Entry.TaxCategory)
	assertEqual(t, "tx1 amount unchanged", "300", entries[0].Entry.TaxableAmount)
	assertEqual(t, "tx2 still taxable", CatPrivateVeraeusserung, entries[1].Entry.TaxCategory)
}

func TestScenario_GainsAndLosses_NetUnderFreigrenze(t *testing.T) {
	// This was the critical bug — +800 gain - 400 loss = net 400 → should apply!
	entries := []RecordEntry{
		{PnL: d("800"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "800",
		}},
		{PnL: d("-400"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx2", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "-400",
		}},
	}

	result := ApplyFreigrenze(entries, 2023)

	assertTrue(t, "Freigrenze applies (net 400 <= 600)", result.ShouldApply)
	assertEqual(t, "tx1 exempt", CatExemptFreigrenze, entries[0].Entry.TaxCategory)
	assertEqual(t, "tx1 taxable=0", "0", entries[0].Entry.TaxableAmount)
	assertEqual(t, "tx2 exempt", CatExemptFreigrenze, entries[1].Entry.TaxCategory)
	assertEqual(t, "tx2 taxable=0", "0", entries[1].Entry.TaxableAmount)
}

func TestScenario_Year2024_HigherThreshold(t *testing.T) {
	// 800€ net gains: over 600€ (2023) but under 1000€ (2024)
	entries := []RecordEntry{
		{PnL: d("800"), IsShort: true, Entry: tax.PluginReportEntry{
			TxID: "tx1", TaxCategory: CatPrivateVeraeusserung, TaxableAmount: "800",
		}},
	}

	result2023 := ApplyFreigrenze(entries, 2023)
	assertFalse(t, "2023: 800 > 600", result2023.ShouldApply)

	// Reset entry
	entries[0].Entry.TaxCategory = CatPrivateVeraeusserung
	entries[0].Entry.TaxableAmount = "800"

	result2024 := ApplyFreigrenze(entries, 2024)
	assertTrue(t, "2024: 800 <= 1000", result2024.ShouldApply)
	assertEqual(t, "exempt for 2024", CatExemptFreigrenze, entries[0].Entry.TaxCategory)
}

func TestScenario_SalePnLWithMixedLots_CorrectCategories(t *testing.T) {
	// Mixed-lot sale should produce two separate results with correct categories
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "5", CostBasis: "500", Fee: "25", HoldingDays: 30},
		{LotID: "lot2", Amount: "5", CostBasis: "1000", Fee: "50", HoldingDays: 400},
	}

	result := computeSalePnLForTest(d("3000"), matches, d("15"))

	shortCat := DetermineSaleCategory(result.Short.IsShort)
	longCat := DetermineSaleCategory(result.Long.IsShort)

	assertEqual(t, "short category", CatPrivateVeraeusserung, shortCat)
	assertEqual(t, "long category", CatExemptLongTerm, longCat)
}

// --- Helper assertions ---

func assertDecimal(t *testing.T, label string, want, got decimal.Decimal) {
	t.Helper()

	if !want.Equal(got) {
		t.Errorf("%s: want %s, got %s", label, want, got)
	}
}

func assertTrue(t *testing.T, label string, got bool) {
	t.Helper()

	if !got {
		t.Errorf("%s: want true, got false", label)
	}
}

func assertFalse(t *testing.T, label string, got bool) {
	t.Helper()

	if got {
		t.Errorf("%s: want false, got true", label)
	}
}

func assertEqual[T comparable](t *testing.T, label string, want, got T) {
	t.Helper()

	if want != got {
		t.Errorf("%s: want %v, got %v", label, want, got)
	}
}

func assertNotNil[T any](t *testing.T, label string, got *T) {
	t.Helper()

	if got == nil {
		t.Fatalf("%s: want non-nil, got nil", label)
	}
}

func assertNil[T any](t *testing.T, label string, got *T) {
	t.Helper()

	if got != nil {
		t.Errorf("%s: want nil, got non-nil", label)
	}
}

// --- ComputeDerivativeLongClosePnL ---

func TestDerivativeLongClose_BasicProfit(t *testing.T) {
	// Buy 1 BTC-PERP at 10000€, sell at 11000€, open fee 5€, close fee 10€.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "10000", Fee: "5"},
	}

	result := ComputeDerivativeLongClosePnL(d("11000"), matches, d("10"))

	// PnL = 11000 - 10000 - 5 - 10 = 985
	assertDecimal(t, "PnL", d("985"), result.PnL)
	// CostBasis = 10000 + 5 + 10 = 10015
	assertDecimal(t, "CostBasis", d("10015"), result.ReportCostBasis)
	// Proceeds = 11000
	assertDecimal(t, "Proceeds", d("11000"), result.ReportProceeds)
	// Verify: Proceeds - CostBasis = PnL
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeLongClose_Loss(t *testing.T) {
	// Buy 2 BTC-PERP at 15000€ each, sell at 14000€ each.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "2", CostBasis: "30000", Fee: "20"},
	}

	result := ComputeDerivativeLongClosePnL(d("28000"), matches, d("15"))

	// PnL = 28000 - 30000 - 20 - 15 = -2035
	assertDecimal(t, "PnL", d("-2035"), result.PnL)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeLongClose_MultipleMatchedLots(t *testing.T) {
	// Two lots at different prices, sold together.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "0.5", CostBasis: "5000", Fee: "3"},
		{LotID: "lot2", Amount: "0.5", CostBasis: "6000", Fee: "4"},
	}

	result := ComputeDerivativeLongClosePnL(d("12500"), matches, d("8"))

	// PnL = 12500 - (5000+6000) - (3+4) - 8 = 12500 - 11000 - 7 - 8 = 1485
	assertDecimal(t, "PnL", d("1485"), result.PnL)
	assertDecimal(t, "CostBasis", d("11015"), result.ReportCostBasis) // 11000 + 7 + 8
	assertDecimal(t, "Proceeds", d("12500"), result.ReportProceeds)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeLongClose_ZeroFees(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "10000", Fee: "0"},
	}

	result := ComputeDerivativeLongClosePnL(d("10500"), matches, d("0"))

	// PnL = 10500 - 10000 - 0 - 0 = 500
	assertDecimal(t, "PnL", d("500"), result.PnL)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

// --- ComputeDerivativeShortClosePnL ---

func TestDerivativeShortClose_BasicProfit(t *testing.T) {
	// Shorted 1 BTC-PERP at 10000€ (stored as CostBasis), buy back at 9000€.
	// Open fee 5€, close fee 10€.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "10000", Fee: "5"},
	}

	result := ComputeDerivativeShortClosePnL(d("9000"), matches, d("10"))

	// PnL = 10000 - 9000 - 5 - 10 = 985
	assertDecimal(t, "PnL", d("985"), result.PnL)
	// ReportProceeds = 10000 - 5 = 9995
	assertDecimal(t, "Proceeds", d("9995"), result.ReportProceeds)
	// ReportCostBasis = 9000 + 10 = 9010
	assertDecimal(t, "CostBasis", d("9010"), result.ReportCostBasis)
	// Verify: Proceeds - CostBasis = PnL
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeShortClose_Loss(t *testing.T) {
	// Shorted at 10000€, buy back at 11000€ (price went up → loss).
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "10000", Fee: "5"},
	}

	result := ComputeDerivativeShortClosePnL(d("11000"), matches, d("10"))

	// PnL = 10000 - 11000 - 5 - 10 = -1015
	assertDecimal(t, "PnL", d("-1015"), result.PnL)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeShortClose_MultipleMatchedLots(t *testing.T) {
	// Two short lots at different prices, closed together.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "100", CostBasis: "500", Fee: "3"},
		{LotID: "lot2", Amount: "200", CostBasis: "1200", Fee: "7"},
	}

	result := ComputeDerivativeShortClosePnL(d("1600"), matches, d("12"))

	// totalShortProceeds = 500 + 1200 = 1700
	// totalOpenFees = 3 + 7 = 10
	// PnL = 1700 - 1600 - 10 - 12 = 78
	assertDecimal(t, "PnL", d("78"), result.PnL)
	// Proceeds = 1700 - 10 = 1690
	assertDecimal(t, "Proceeds", d("1690"), result.ReportProceeds)
	// CostBasis = 1600 + 12 = 1612
	assertDecimal(t, "CostBasis", d("1612"), result.ReportCostBasis)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeShortClose_ZeroFees(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "10000", Fee: "0"},
	}

	result := ComputeDerivativeShortClosePnL(d("9500"), matches, d("0"))

	// PnL = 10000 - 9500 = 500
	assertDecimal(t, "PnL", d("500"), result.PnL)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeShortClose_FlatPriceNoProfit(t *testing.T) {
	// Short and close at same price → only fees create a loss.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", Amount: "1", CostBasis: "10000", Fee: "10"},
	}

	result := ComputeDerivativeShortClosePnL(d("10000"), matches, d("10"))

	// PnL = 10000 - 10000 - 10 - 10 = -20
	assertDecimal(t, "PnL", d("-20"), result.PnL)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

// --- AllocateFee ---

func TestAllocateFee_FullMatch(t *testing.T) {
	result := AllocateFee(d("10"), d("5"), d("5"))
	assertDecimal(t, "Full match", d("10"), result)
}

func TestAllocateFee_PartialMatch(t *testing.T) {
	// 3 out of 10 matched → fee = 10 * 3/10 = 3
	result := AllocateFee(d("10"), d("3"), d("10"))
	assertDecimal(t, "Partial match", d("3"), result)
}

func TestAllocateFee_ZeroMatched(t *testing.T) {
	result := AllocateFee(d("10"), d("0"), d("5"))
	assertDecimal(t, "Zero matched", d("0"), result)
}

func TestAllocateFee_ZeroTotal(t *testing.T) {
	result := AllocateFee(d("10"), d("5"), d("0"))
	assertDecimal(t, "Zero total", d("0"), result)
}

// --- AllocateProceeds ---

func TestAllocateProceeds_FullMatch(t *testing.T) {
	result := AllocateProceeds(d("1000"), d("1"), d("1"))
	assertDecimal(t, "Full match", d("1000"), result)
}

func TestAllocateProceeds_PartialMatch(t *testing.T) {
	// 1 out of 4 matched → proceeds = 1000 * 1/4 = 250
	result := AllocateProceeds(d("1000"), d("1"), d("4"))
	assertDecimal(t, "Partial match", d("250"), result)
}

func TestAllocateProceeds_ZeroMatched(t *testing.T) {
	result := AllocateProceeds(d("1000"), d("0"), d("5"))
	assertDecimal(t, "Zero matched", d("0"), result)
}

// --- Round-trip scenarios ---

func TestDerivativeRoundTrip_LongProfitable(t *testing.T) {
	// Full round-trip: BUY 0.5 BTC-PERP at €20000 (cost 10000), SELL at €22000 (proceeds 11000).
	// Open fee €6, close fee €8.
	// Simulates: BUY adds lot {CostBasis=10000/0.5=20000 per-unit, Fee=6/0.5=12 per-unit}
	// then SELL disposes it.
	matches := []tax.FifoLotMatch{
		{LotID: "buy1", Amount: "0.5", CostBasis: "10000", Fee: "6"},
	}
	result := ComputeDerivativeLongClosePnL(d("11000"), matches, d("8"))
	// PnL = 11000 - 10000 - 6 - 8 = 986
	assertDecimal(t, "Long round-trip PnL", d("986"), result.PnL)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeRoundTrip_ShortProfitable(t *testing.T) {
	// Full round-trip: SELL 0.356 BTC-PERP at €18710 (proceeds €6661.16), BUY back at €17000 (cost €6052).
	// Open fee €4.66, close fee €4.24.
	matches := []tax.FifoLotMatch{
		{LotID: "sell1", Amount: "0.356", CostBasis: "6661.16", Fee: "4.66"},
	}
	result := ComputeDerivativeShortClosePnL(d("6052"), matches, d("4.24"))
	// PnL = 6661.16 - 6052 - 4.66 - 4.24 = 600.26
	assertDecimal(t, "Short round-trip PnL", d("600.26"), result.PnL)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestDerivativeRoundTrip_ShortLoss(t *testing.T) {
	// SELL 0.356 BTC-PERP at €18710 (proceeds €6661.16), BUY back at €19246 (cost €6851.58).
	// Open fee €4.66, close fee €4.80.
	matches := []tax.FifoLotMatch{
		{LotID: "sell1", Amount: "0.356", CostBasis: "6661.16", Fee: "4.66"},
	}
	result := ComputeDerivativeShortClosePnL(d("6851.58"), matches, d("4.80"))
	// PnL = 6661.16 - 6851.58 - 4.66 - 4.80 = -199.88
	assertDecimal(t, "Short loss PnL", d("-199.88"), result.PnL)
	assertDecimal(t, "Proceeds-CostBasis=PnL", result.PnL, result.ReportProceeds.Sub(result.ReportCostBasis))
}

func TestAllocateFee_PrecisionWithSmallRatio(t *testing.T) {
	// 0.001 out of 10 → fee = 50 * 0.001/10 = 0.005
	result := AllocateFee(d("50"), d("0.001"), d("10"))
	assertDecimal(t, "Small ratio", d("0.005"), result)
}

// --- ComputeShortClosePnLAt (margin-spot short close with holding period splitting) ---

func TestShortClosePnLAt_AllShortTerm(t *testing.T) {
	// Short sold at 10000€, bought back at 9000€, held < 1 year.
	// Open fee 5€, close fee 10€.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", LotTs: "2021-03-01T00:00:00Z", Amount: "1", CostBasis: "10000", Fee: "5", HoldingDays: 30},
	}

	result := ComputeShortClosePnLAt(d("9000"), matches, d("10"), "2021-03-31T00:00:00Z")

	// All short-term
	assertNotNil(t, "Short bucket", result.Short)
	assertNil(t, "Long bucket", result.Long)

	// PnL = 10000 - 9000 - 5 - 10 = 985
	assertDecimal(t, "PnL", d("985"), result.Short.PnL)
	// Proceeds = 10000 - 5 = 9995 (net short-open proceeds)
	assertDecimal(t, "Proceeds", d("9995"), result.Short.Proceeds)
	// CostBasis = 9000 (buy-back), TotalFees = 10€ (close fee)
	assertDecimal(t, "CostBasis", d("9000"), result.Short.TotalCostBasis)
	assertDecimal(t, "TotalFees", d("10"), result.Short.TotalFees)
	assertTrue(t, "IsShort", result.Short.IsShort)
	// Verify: Proceeds - CostBasis - TotalFees = PnL
	assertDecimal(t, "Proceeds-Cost-Fees=PnL", result.Short.PnL,
		result.Short.Proceeds.Sub(result.Short.TotalCostBasis).Sub(result.Short.TotalFees))
}

func TestShortClosePnLAt_AllLongTerm(t *testing.T) {
	// Short held > 1 year → long-term exempt.
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", LotTs: "2020-01-01T00:00:00Z", Amount: "1", CostBasis: "10000", Fee: "5", HoldingDays: 400},
	}

	result := ComputeShortClosePnLAt(d("9000"), matches, d("10"), "2021-02-05T00:00:00Z")

	assertNil(t, "Short bucket", result.Short)
	assertNotNil(t, "Long bucket", result.Long)
	assertDecimal(t, "PnL", d("985"), result.Long.PnL)
	assertFalse(t, "IsShort", result.Long.IsShort)
}

func TestShortClosePnLAt_MixedHoldingPeriods(t *testing.T) {
	// Two short lots: one held < 1 year, one held > 1 year. Both closed together.
	// buyBackCost = 18000 total (for 2 units).
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", LotTs: "2021-06-01T00:00:00Z", Amount: "1", CostBasis: "10000", Fee: "5", HoldingDays: 60},
		{LotID: "lot2", LotTs: "2020-01-01T00:00:00Z", Amount: "1", CostBasis: "11000", Fee: "8", HoldingDays: 600},
	}

	result := ComputeShortClosePnLAt(d("18000"), matches, d("20"), "2021-08-01T00:00:00Z")

	assertNotNil(t, "Short bucket", result.Short)
	assertNotNil(t, "Long bucket", result.Long)

	// Short bucket (lot1): allocated buyBack = 18000 * 1/2 = 9000, closeFee = 20 * 1/2 = 10
	// PnL = 10000 - 9000 - 5 - 10 = 985
	assertDecimal(t, "Short PnL", d("985"), result.Short.PnL)
	assertTrue(t, "Short IsShort", result.Short.IsShort)

	// Long bucket (lot2): allocated buyBack = 18000 * 1/2 = 9000, closeFee = 20 * 1/2 = 10
	// PnL = 11000 - 9000 - 8 - 10 = 1982
	assertDecimal(t, "Long PnL", d("1982"), result.Long.PnL)
	assertFalse(t, "Long IsShort", result.Long.IsShort)

	// Combined PnL = 985 + 1982 = 2967
	combined := result.Short.PnL.Add(result.Long.PnL)
	assertDecimal(t, "Combined PnL", d("2967"), combined)
}

func TestShortClosePnLAt_Loss(t *testing.T) {
	// Short sold at 10000€, bought back at 11000€ (price went up → loss).
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", LotTs: "2021-03-01T00:00:00Z", Amount: "1", CostBasis: "10000", Fee: "5", HoldingDays: 30},
	}

	result := ComputeShortClosePnLAt(d("11000"), matches, d("10"), "2021-03-31T00:00:00Z")

	assertNotNil(t, "Short bucket", result.Short)
	// PnL = 10000 - 11000 - 5 - 10 = -1015
	assertDecimal(t, "PnL", d("-1015"), result.Short.PnL)
	assertDecimal(t, "Proceeds-Cost-Fees=PnL", result.Short.PnL,
		result.Short.Proceeds.Sub(result.Short.TotalCostBasis).Sub(result.Short.TotalFees))
}

func TestShortClosePnLAt_ZeroFees(t *testing.T) {
	matches := []tax.FifoLotMatch{
		{LotID: "lot1", LotTs: "2021-03-01T00:00:00Z", Amount: "0.5", CostBasis: "5000", Fee: "0", HoldingDays: 30},
	}

	result := ComputeShortClosePnLAt(d("4500"), matches, d("0"), "2021-03-31T00:00:00Z")

	assertDecimal(t, "PnL", d("500"), result.Short.PnL)
	assertDecimal(t, "Proceeds-Cost-Fees=PnL", result.Short.PnL,
		result.Short.Proceeds.Sub(result.Short.TotalCostBasis).Sub(result.Short.TotalFees))
}
