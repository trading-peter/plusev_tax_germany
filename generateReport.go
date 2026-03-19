package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/plusev-terminal/go-plugin-common/plugin"
	"github.com/plusev-terminal/go-plugin-common/tax"
	"github.com/shopspring/decimal"
	"github.com/trading-peter/plusev_tax_germany/taxcalc"
)

const pageSize = 500

// costBasisCurrency is the fiat currency used for cost-basis tracking (e.g. "EUR").
// Set once at report start, used by processTrade to skip quote-lot tracking when
// the quote already is the cost-basis currency (no FIFO needed for fiat).
var costBasisCurrency string

// deMinimis is the minimum EUR value for phantom inventory warnings.
// Unmatched amounts worth less than this are suppressed as dust.
var deMinimis = decimal.NewFromFloat(0.01)

func (p *GermanTaxPlugin) handleGenerateReport(params map[string]any) plugin.Response {
	reportParams := tax.GenerateReportParamsFromMap(params)
	if err := reportParams.Validate(); err != nil {
		return plugin.ErrorResponse(err)
	}
	taxYear := reportParams.TaxYear

	// Extract run_id for document storage.
	var runID int
	if rid, ok := params["run_id"]; ok {
		switch v := rid.(type) {
		case float64:
			runID = int(v)
		case int:
			runID = v
		}
	}

	// Define the tax period using German local time (CET = UTC+1).
	// Records store UTC timestamps, but German tax years are based on local time.
	// Jan 1 and Dec 31 are always in CET (not CEST), so a fixed UTC+1 offset is correct.
	cet := time.FixedZone("CET", 1*60*60)
	from := time.Date(taxYear, 1, 1, 0, 0, 0, 0, cet).UTC()
	to := time.Date(taxYear, 12, 31, 23, 59, 59, 0, cet).UTC()

	log.InfoWithData("Starting German tax report", map[string]any{
		"taxYear": taxYear,
		"from":    from.Format(time.RFC3339),
		"to":      to.Format(time.RFC3339),
	})

	// Get settings to confirm cost basis currency.
	settings, err := tax.GetSettings()
	if err != nil {
		return plugin.ErrorResponse(fmt.Errorf("get_settings failed: %w", err))
	}

	costBasisCurrency = settings.CostBasisCurrency

	if settings.CostBasisCurrency != "EUR" {
		log.WarnWithData("Cost basis currency is not EUR — German report expects EUR", map[string]any{
			"currency": settings.CostBasisCurrency,
		})
	}

	// Try to restore FIFO state from a previous run (e.g. prior year).
	prevStateKey := fmt.Sprintf("fifo_%d", taxYear-1)
	found, err := tax.FifoRestoreState(prevStateKey)
	if err != nil {
		log.WarnWithData("Failed to restore FIFO state, starting fresh", map[string]any{
			"key":   prevStateKey,
			"error": err.Error(),
		})
	}

	if found {
		log.InfoWithData("Restored FIFO state from previous year", map[string]any{"key": prevStateKey})
	} else {
		// No prior state — reset to ensure clean slate.
		if err := tax.FifoReset(); err != nil {
			return plugin.ErrorResponse(fmt.Errorf("fifo_reset failed: %w", err))
		}
	}

	// Phase 1: Count total records for progress reporting.
	countResp, err := tax.GetRecords(tax.PluginGetRecordsRequest{
		From:     from.Format(time.RFC3339),
		To:       to.Format(time.RFC3339),
		Page:     1,
		PageSize: 1,
	})
	if err != nil {
		return plugin.ErrorResponse(fmt.Errorf("get_records count failed: %w", err))
	}
	totalRecords := int(countResp.TotalCount)

	// Phase 2: Process all records.
	var entries []taxcalc.RecordEntry
	processed := 0

	_ = tax.ReportProgress(tax.PluginReportProgress{
		Phase:   "processing",
		Message: fmt.Sprintf("Processing %d records", totalRecords),
		Total:   totalRecords,
	})

	page := 1
	for {
		resp, err := tax.GetRecords(tax.PluginGetRecordsRequest{
			From:     from.Format(time.RFC3339),
			To:       to.Format(time.RFC3339),
			Page:     page,
			PageSize: pageSize,
		})
		if err != nil {
			return plugin.ErrorResponse(fmt.Errorf("get_records page %d failed: %w", page, err))
		}

		for _, rec := range resp.Records {
			results, err := processRecord(rec)
			if err != nil {
				log.WarnWithData("Error processing record, skipping", map[string]any{
					"txId":  rec.TxID,
					"error": err.Error(),
				})
			}

			// Decorate entries with metadata from the source record for document generation.
			feeC, _ := decimal.NewFromString(rec.FeeC)

			for i := range results {
				results[i].Ticker = rec.Ticker
				results[i].Quote = rec.Quote
				results[i].Action = rec.Action
				results[i].IsDerivative = rec.IsDerivative || rec.IsMarginTrade
				results[i].TransferCategory = rec.TransferCategory
				results[i].FeeC = feeC

				// Determine account: for transfers, use source (fee) or destination (income).
				account := rec.Account

				if rec.RecordType == "transfer" {
					if results[i].Entry.TaxCategory == taxcalc.CatSonstigeEinkuenfte {
						if rec.Destination != "" {
							account = rec.Destination
						}
					} else {
						if rec.Source != "" {
							account = rec.Source
						}
					}
				}

				results[i].Account = account
			}

			entries = append(entries, results...)

			processed++
			if processed%100 == 0 {
				_ = tax.ReportProgress(tax.PluginReportProgress{
					Phase:     "processing",
					Processed: processed,
					Total:     totalRecords,
				})
			}
		}

		if page >= resp.TotalPages {
			break
		}
		page++
	}

	_ = tax.ReportProgress(tax.PluginReportProgress{
		Phase:     "calculating",
		Processed: processed,
		Total:     totalRecords,
		Message:   "Applying Freigrenze",
	})

	// Phase 3: Apply 600€ Freigrenze and submit entries.
	freigrenzeResult := taxcalc.ApplyFreigrenze(entries, taxYear)

	// Phase 4: Submit report entries.
	submitted := 0
	for _, e := range entries {
		if err := tax.SubmitReportEntry(e.Entry); err != nil {
			log.WarnWithData("Failed to submit report entry", map[string]any{
				"txId":  e.Entry.TxID,
				"error": err.Error(),
			})
			continue
		}

		submitted++
	}

	// Phase 5: Save FIFO state for next year.
	stateKey := fmt.Sprintf("fifo_%d", taxYear)
	if err := tax.FifoSaveState(stateKey); err != nil {
		log.WarnWithData("Failed to save FIFO state", map[string]any{
			"key":   stateKey,
			"error": err.Error(),
		})
	}

	// Phase 6: Compute and submit summary.
	summary := computeSummary(entries, freigrenzeResult, taxYear, settings.CostBasisCurrency)
	if err := tax.SubmitReportSummary(summary); err != nil {
		log.WarnWithData("Failed to submit report summary", map[string]any{
			"error": err.Error(),
		})
	}

	monthlySummaries := computeMonthlySummaries(entries, freigrenzeResult, taxYear, settings.CostBasisCurrency)
	if err := tax.SubmitReportMonthlySummaries(monthlySummaries); err != nil {
		log.WarnWithData("Failed to submit monthly report summaries", map[string]any{
			"error": err.Error(),
		})
	}

	// Phase 7: Generate and store report documents (HTML).
	_ = tax.ReportProgress(tax.PluginReportProgress{
		Phase:     "documents",
		Processed: totalRecords,
		Total:     totalRecords,
		Message:   "Generating report documents",
	})

	if runID > 0 {
		if err := generateDocuments(entries, taxYear, settings.CostBasisCurrency, runID); err != nil {
			log.WarnWithData("Failed to generate report documents", map[string]any{
				"error": err.Error(),
			})
		}
	}

	_ = tax.ReportProgress(tax.PluginReportProgress{
		Phase:     "finalizing",
		Processed: totalRecords,
		Total:     totalRecords,
		Message:   "Report complete",
	})

	log.InfoWithData("German tax report complete", map[string]any{
		"taxYear":           taxYear,
		"totalRecords":      totalRecords,
		"entriesGenerated":  submitted,
		"netShortTermPnL":   freigrenzeResult.NetShortTermPnL.String(),
		"freigrenzeApplied": freigrenzeResult.ShouldApply,
	})

	return plugin.SuccessResponse(map[string]any{
		"taxYear":           taxYear,
		"totalRecords":      totalRecords,
		"entriesGenerated":  submitted,
		"netShortTermPnL":   freigrenzeResult.NetShortTermPnL.String(),
		"freigrenzeApplied": freigrenzeResult.ShouldApply,
	})
}

// processRecord routes a single HistoryRecord through the appropriate tax logic.
func processRecord(rec tax.PluginRecord) ([]taxcalc.RecordEntry, error) {
	checkMissingConversions(rec)

	switch rec.RecordType {
	case "trade":
		return processTrade(rec)
	case "transfer":
		return processTransfer(rec)
	default:
		return nil, fmt.Errorf("unknown record type: %s", rec.RecordType)
	}
}

// checkMissingConversions emits a warning if a record is missing cost-basis
// conversion fields that are needed for accurate tax calculations.
func checkMissingConversions(rec tax.PluginRecord) {
	var missing []string

	switch rec.RecordType {
	case "trade":
		// If the asset is the cost-basis currency itself, conversions are trivial (1:1).
		if rec.Asset == costBasisCurrency {
			break
		}
		if rec.PriceC == "" || rec.PriceC == "0" {
			missing = append(missing, "price_c")
		}
		if rec.ValueC == "" || rec.ValueC == "0" {
			missing = append(missing, "value_c")
		}
		fee, _ := decimal.NewFromString(rec.Fee)
		if fee.IsPositive() && (rec.FeeC == "" || rec.FeeC == "0") {
			missing = append(missing, "fee_c")
		}
	case "transfer":
		// Uncategorized transfers don't need price_c (no FIFO lot, no income entry).
		// airdrop_exempt doesn't need price_c (cost basis is always 0).
		if rec.TransferCategory != "" && rec.TransferCategory != "airdrop_exempt" && (rec.PriceC == "" || rec.PriceC == "0") {
			missing = append(missing, "price_c")
		}
		fee, _ := decimal.NewFromString(rec.Fee)
		if fee.IsPositive() && (rec.FeeC == "" || rec.FeeC == "0") {
			missing = append(missing, "fee_c")
		}
	}

	if len(missing) > 0 {
		log.WarnWithData("Record is missing cost-basis conversion(s)", map[string]any{
			"uid":        rec.UID,
			"txId":       rec.TxID,
			"recordType": rec.RecordType,
			"ts":         rec.Ts,
			"asset":      rec.Asset,
			"missing":    missing,
		})
	}
}

// processTrade handles BUY and SELL trades.
func processTrade(rec tax.PluginRecord) ([]taxcalc.RecordEntry, error) {
	amount, _ := decimal.NewFromString(rec.Amount)
	priceC, _ := decimal.NewFromString(rec.PriceC)
	valueC, _ := decimal.NewFromString(rec.ValueC)
	feeC, _ := decimal.NewFromString(rec.FeeC)
	feePriceC, _ := decimal.NewFromString(rec.FeePriceC)

	if amount.IsZero() {
		return nil, nil
	}

	// If the asset is the cost-basis currency (e.g. EUR), the conversion is 1:1.
	if rec.Asset == costBasisCurrency {
		priceC = decimal.NewFromInt(1)
		valueC = amount
	}

	// Derivatives (e.g. perpetual futures): position-matched PnL (§ 20 EStG).
	// Uses dual FIFO (long/short) since derivatives can be opened in either direction.
	if rec.IsDerivative {
		return processDerivative(rec)
	}

	// Margin spot trades (IsMarginTrade, not IsDerivative): actual delivery of
	// crypto, but can sell-before-buy (borrow + sell spot). Uses dual FIFO like
	// derivatives but with § 23 EStG treatment (1-year exemption, Freigrenze).
	if rec.IsMarginTrade {
		return processMarginSpot(rec)
	}

	// Action values: BUY=0, SELL=1 (from types.TradeAction).
	isBuy := rec.Action == 0

	if isBuy {
		perUnitCost := taxcalc.ComputePerUnitCost(valueC, amount, priceC)
		perUnitFee := taxcalc.ComputePerUnitFee(feeC, amount)

		err := tax.FifoAddLot(tax.FifoAddLotRequest{
			Account:   rec.Account,
			Asset:     rec.Asset,
			ID:        rec.TxID,
			Ts:        rec.Ts,
			Amount:    amount.String(),
			CostBasis: perUnitCost.String(),
			Fee:       perUnitFee.String(),
			Metadata:  map[string]any{"ticker": rec.Ticker},
		})
		if err != nil {
			return nil, fmt.Errorf("fifo_add_lot failed: %w", err)
		}

		// Dispose quote currency lots (e.g. ETH spent to buy FXI).
		// Skip if the quote is the cost-basis currency (fiat) — no FIFO for fiat.
		// Crypto-to-crypto swaps are taxable: disposing the quote is a §23 event.
		var results []taxcalc.RecordEntry

		if rec.Quote != "" && rec.Quote != costBasisCurrency {
			quoteAmount, _ := decimal.NewFromString(rec.Value)
			if quoteAmount.IsPositive() {
				quoteDisposal, err := tax.FifoDispose(tax.FifoDisposeRequest{
					Account: rec.Account,
					Asset:   rec.Quote,
					Ts:      rec.Ts,
					Amount:  quoteAmount.String(),
				})
				if err != nil {
					log.WarnWithData("quote currency dispose failed (buy)", map[string]any{
						"txId":  rec.TxID,
						"quote": rec.Quote,
						"error": err.Error(),
					})
				} else {
					quotePriceC, _ := decimal.NewFromString(rec.QuotePriceC)

					unmatchedQuote, _ := decimal.NewFromString(quoteDisposal.UnmatchedAmount)
					if unmatchedQuote.IsPositive() {
						unmatchedValue := unmatchedQuote.Mul(quotePriceC)
						if unmatchedValue.GreaterThanOrEqual(deMinimis) {
							log.WarnWithData("Unmatched quote disposal (phantom inventory)", map[string]any{
								"txId":      rec.TxID,
								"quote":     rec.Quote,
								"account":   rec.Account,
								"unmatched": unmatchedQuote.String(),
							})
						}
					}

					// Compute PnL for the quote currency disposal.
					// Proceeds = EUR value of the quote amount spent.
					// Only attribute proceeds proportional to the matched amount,
					// not the full quote amount — otherwise unmatched (phantom)
					// inventory inflates PnL on the tiny matched dust.
					matchedQuoteAmount, _ := decimal.NewFromString(quoteDisposal.MatchedAmount)
					quoteProceeds := matchedQuoteAmount.Mul(quotePriceC)
					if quoteProceeds.IsZero() && matchedQuoteAmount.IsPositive() {
						// Fallback: use the trade's EUR value scaled by matched ratio.
						if quoteAmount.IsPositive() {
							quoteProceeds = valueC.Mul(matchedQuoteAmount).Div(quoteAmount)
						}
					}

					// Fee is zero — the trade fee is already in the buy lot's cost basis.
					quoteSale := taxcalc.ComputeSalePnLAt(quoteProceeds, quoteDisposal.Matches, decimal.Zero, rec.Ts)

					quoteDetails := marshalDetails(map[string]any{
						"type":            "quote_disposal",
						"matches":         quoteDisposal.Matches,
						"matchedAmount":   quoteDisposal.MatchedAmount,
						"unmatchedAmount": quoteDisposal.UnmatchedAmount,
						"quoteProceeds":   quoteProceeds.String(),
					})

					for _, bucket := range []*taxcalc.SaleResult{quoteSale.Short, quoteSale.Long} {
						if bucket == nil {
							continue
						}

						category := taxcalc.DetermineSaleCategory(bucket.IsShort)

						taxableAmount := bucket.PnL.String()
						if category == taxcalc.CatExemptLongTerm {
							taxableAmount = "0"
						}

						results = append(results, taxcalc.RecordEntry{
							Entry: tax.PluginReportEntry{
								TxID:          rec.TxID + ":quote-disposal",
								RecordType:    "trade",
								Ts:            rec.Ts,
								Asset:         rec.Quote,
								Amount:        bucket.Amount.String(),
								PnL:           bucket.PnL.String(),
								HoldingPeriod: bucket.HoldingDays,
								TaxCategory:   category,
								TaxableAmount: taxableAmount,
								CostBasis:     bucket.TotalCostBasis.Add(bucket.TotalFees).String(),
								Proceeds:      bucket.Proceeds.String(),
								Details:       quoteDetails,
							},
							PnL:     bucket.PnL,
							IsShort: bucket.IsShort,
						})
					}
				}
			}
		}

		return results, nil
	}

	// SELL — dispose via FIFO and compute PnL.
	disposal, err := tax.FifoDispose(tax.FifoDisposeRequest{
		Account: rec.Account,
		Asset:   rec.Asset,
		Ts:      rec.Ts,
		Amount:  amount.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("fifo_dispose failed: %w", err)
	}

	proceeds := taxcalc.ComputeProceeds(valueC, amount, priceC)
	sale := taxcalc.ComputeSalePnLAt(proceeds, disposal.Matches, feeC, rec.Ts)

	// Add quote currency lots (e.g. USDT received from selling the asset).
	// Skip if the quote is the cost-basis currency (fiat) — no FIFO for fiat.
	if rec.Quote != "" && rec.Quote != costBasisCurrency {
		quoteAmount, _ := decimal.NewFromString(rec.Value)
		if quoteAmount.IsPositive() {
			quotePriceC, _ := decimal.NewFromString(rec.QuotePriceC)
			if quotePriceC.IsZero() && quoteAmount.IsPositive() {
				// Derive from valueC / quoteAmount as fallback.
				quotePriceC = valueC.Div(quoteAmount)
			}
			err := tax.FifoAddLot(tax.FifoAddLotRequest{
				Account:   rec.Account,
				Asset:     rec.Quote,
				ID:        rec.TxID + ":quote",
				Ts:        rec.Ts,
				Amount:    quoteAmount.String(),
				CostBasis: quotePriceC.String(),
				Fee:       "0",
				Metadata:  map[string]any{"ticker": rec.Ticker, "quote_lot": true},
			})
			if err != nil {
				log.WarnWithData("quote currency add lot failed (sell)", map[string]any{
					"txId":  rec.TxID,
					"quote": rec.Quote,
					"error": err.Error(),
				})
			}
		}
	}

	// Check for unmatched amount (phantom inventory).
	unmatchedAmount, _ := decimal.NewFromString(disposal.UnmatchedAmount)
	if unmatchedAmount.GreaterThan(decimal.Zero) {
		// Suppress dust: only warn if unmatched value >= €0.01.
		unmatchedValue := unmatchedAmount.Mul(priceC)
		if unmatchedValue.GreaterThanOrEqual(deMinimis) {
			log.WarnWithData("Unmatched disposal amount (phantom inventory)", map[string]any{
				"txId":      rec.TxID,
				"asset":     rec.Asset,
				"account":   rec.Account,
				"unmatched": unmatchedAmount.String(),
			})
		}
	}

	details := marshalDetails(map[string]any{
		"matches":         disposal.Matches,
		"matchedAmount":   disposal.MatchedAmount,
		"unmatchedAmount": disposal.UnmatchedAmount,
		"fee":             feeC.String(),
		"feeCurrency":     rec.FeeCurrency,
		"feePriceC":       feePriceC.String(),
	})

	// Build entries for each holding-period bucket (short-term and/or long-term).
	var results []taxcalc.RecordEntry

	for _, bucket := range []*taxcalc.SaleResult{sale.Short, sale.Long} {
		if bucket == nil {
			continue
		}

		category := taxcalc.DetermineSaleCategory(bucket.IsShort)

		taxableAmount := bucket.PnL.String()
		if category == taxcalc.CatExemptLongTerm {
			taxableAmount = "0"
		}

		results = append(results, taxcalc.RecordEntry{
			Entry: tax.PluginReportEntry{
				TxID:          rec.TxID,
				RecordType:    "trade",
				Ts:            rec.Ts,
				Asset:         rec.Asset,
				Amount:        bucket.Amount.String(),
				PnL:           bucket.PnL.String(),
				HoldingPeriod: bucket.HoldingDays,
				TaxCategory:   category,
				TaxableAmount: taxableAmount,
				CostBasis:     bucket.TotalCostBasis.Add(bucket.TotalFees).String(),
				Proceeds:      bucket.Proceeds.String(),
				Details:       details,
			},
			PnL:     bucket.PnL,
			IsShort: bucket.IsShort,
		})
	}

	return results, nil
}

// processTransfer handles DEPOSIT and WITHDRAWAL transfers.
// Transfers move lots between wallets without triggering a taxable event
// (except for the fee, which is a disposal).
// Deposits with a transfer category (staking_reward, mining, airdrop, etc.) add FIFO lots
// so the cost basis is tracked when those coins are later sold.
func processTransfer(rec tax.PluginRecord) ([]taxcalc.RecordEntry, error) {
	// Internal transfers (e.g. DEX escrow movements) are not taxable events
	// and should not affect FIFO inventory at all.
	if rec.TransferCategory == "internal" {
		return nil, nil
	}

	var results []taxcalc.RecordEntry

	amount, _ := decimal.NewFromString(rec.Amount)
	if amount.IsZero() {
		return nil, nil
	}

	// Action: DEPOSIT=0, WITHDRAWAL=1 (from types.TransferAction)
	isWithdrawal := rec.Action == 1
	isDeposit := rec.Action == 0

	if isWithdrawal && rec.Source != "" && rec.Destination != "" {
		// Move lots from source to destination, preserving cost basis + timestamps.
		// MoveLots is partial: it moves whatever is available and reports any shortfall.
		moveResp, err := tax.FifoMoveLots(tax.FifoMoveLotsRequest{
			FromAccount: rec.Source,
			ToAccount:   rec.Destination,
			Asset:       rec.Asset,
			Amount:      amount.String(),
		})
		if err != nil {
			log.WarnWithData("fifo_move_lots failed", map[string]any{
				"txId":  rec.TxID,
				"error": err.Error(),
			})
		}

		// For any shortfall, create a lot at market price (Zeitwert).
		// This happens when margin-spot activity consumed the plain-asset FIFO
		// balance but the exchange still held withdrawable equity.
		shortfall, _ := decimal.NewFromString(moveResp.Shortfall)
		movedAmount, _ := decimal.NewFromString(moveResp.MovedAmount)
		if shortfall.IsPositive() {
			// Use the asset's converted EUR price from the transfer record.
			perUnitCost, _ := decimal.NewFromString(rec.PriceC)

			if perUnitCost.IsZero() {
				log.WarnWithData("No market price available for shortfall lot, cost basis will be zero", map[string]any{
					"txId":      rec.TxID,
					"asset":     rec.Asset,
					"shortfall": shortfall.String(),
				})
			}

			err := tax.FifoAddLot(tax.FifoAddLotRequest{
				Account:   rec.Destination,
				Asset:     rec.Asset,
				ID:        rec.TxID + ":margin-gap",
				Ts:        rec.Ts,
				Amount:    shortfall.String(),
				CostBasis: perUnitCost.String(),
				Fee:       "0",
				Metadata: map[string]any{
					"margin_balance_gap": true,
					"source":             rec.Source,
					"original_amount":    amount.String(),
					"moved_amount":       movedAmount.String(),
				},
			})
			if err != nil {
				log.WarnWithData("Failed to create shortfall lot", map[string]any{
					"txId":  rec.TxID,
					"error": err.Error(),
				})
			} else {
				log.InfoWithData("Created market-price lot for margin balance gap", map[string]any{
					"txId":        rec.TxID,
					"asset":       rec.Asset,
					"shortfall":   shortfall.String(),
					"perUnitCost": perUnitCost.String(),
					"destination": rec.Destination,
				})
			}
		}
	}

	// For deposits with a transfer category (staking_reward, mining, airdrop, etc.),
	// add a FIFO lot so the cost basis is tracked for later disposal.
	// Spam transfers are ignored — they have no taxable value.
	//
	// Two airdrop types per BMF Schreiben 2025:
	//   - "airdrop" (taxable): active Leistung → § 22 Nr. 3 income at receipt, CostBasis = FMV.
	//   - "airdrop_exempt": no Leistung → no income at receipt, CostBasis = 0€.
	// Staking/mining rewards are always § 22 Nr. 3 income at receipt.
	if isDeposit && rec.TransferCategory != "" {
		account := rec.Account
		if rec.Destination != "" {
			account = rec.Destination
		}

		perUnitCost, _ := decimal.NewFromString(rec.PriceC)

		// Tax-exempt airdrops have zero cost basis (no Leistung → no FMV attribution).
		costBasis := perUnitCost
		if rec.TransferCategory == "airdrop_exempt" {
			costBasis = decimal.Zero
		}

		err := tax.FifoAddLot(tax.FifoAddLotRequest{
			Account:   account,
			Asset:     rec.Asset,
			ID:        rec.TxID,
			Ts:        rec.Ts,
			Amount:    amount.String(),
			CostBasis: costBasis.String(),
			Fee:       "0",
			Metadata: map[string]any{
				"transfer_category": rec.TransferCategory,
			},
		})
		if err != nil {
			log.WarnWithData("fifo_add_lot for deposit failed", map[string]any{
				"txId":  rec.TxID,
				"error": err.Error(),
			})
		}

		// Taxable categories generate § 22 Nr. 3 income at receipt.
		// The income equals amount × FMV (PriceC). This is NOT a disposal —
		// it's "sonstige Einkünfte" recognized when the coins are received.
		if rec.TransferCategory != "airdrop_exempt" && perUnitCost.IsPositive() {
			incomeValue := amount.Mul(perUnitCost)

			details := marshalDetails(map[string]any{
				"type":              "deposit_income",
				"transfer_category": rec.TransferCategory,
				"asset":             rec.Asset,
				"amount":            amount.String(),
				"priceC":            perUnitCost.String(),
			})

			results = append(results, taxcalc.RecordEntry{
				Entry: tax.PluginReportEntry{
					TxID:          rec.TxID,
					RecordType:    "transfer",
					Ts:            rec.Ts,
					Asset:         rec.Asset,
					Amount:        amount.String(),
					PnL:           incomeValue.String(),
					HoldingPeriod: 0,
					TaxCategory:   taxcalc.CatSonstigeEinkuenfte,
					TaxableAmount: incomeValue.String(),
					CostBasis:     incomeValue.String(),
					Proceeds:      incomeValue.String(),
					Details:       details,
				},
				PnL:     incomeValue,
				IsShort: true,
			})
		}
	}

	// If there's a fee, dispose it — transfer fees are taxable events.
	feeC, _ := decimal.NewFromString(rec.FeeC)
	if feeC.IsZero() {
		return results, nil
	}

	feeAmount, _ := decimal.NewFromString(rec.Fee)
	if feeAmount.IsZero() {
		return results, nil
	}

	// The fee asset is consumed from the source account.
	feeAccount := rec.Account
	if rec.Source != "" {
		feeAccount = rec.Source
	}

	disposal, err := tax.FifoDispose(tax.FifoDisposeRequest{
		Account: feeAccount,
		Asset:   rec.FeeCurrency,
		Ts:      rec.Ts,
		Amount:  feeAmount.String(),
	})
	if err != nil {
		log.WarnWithData("Failed to dispose transfer fee", map[string]any{
			"txId":  rec.TxID,
			"error": err.Error(),
		})
		return results, nil
	}

	feeResult := taxcalc.ComputeFeePnLAt(feeC, disposal.Matches, rec.Ts)

	details := marshalDetails(map[string]any{
		"type":        "transfer_fee",
		"feeCurrency": rec.FeeCurrency,
		"feeAmount":   feeAmount.String(),
		"feeValueC":   feeC.String(),
	})

	// Build entries for each holding-period bucket (short-term and/or long-term).

	for _, bucket := range []*taxcalc.FeePnLResult{feeResult.Short, feeResult.Long} {
		if bucket == nil {
			continue
		}

		category := taxcalc.DetermineTransferFeeCategory(bucket.IsShort)

		taxableAmount := bucket.PnL.String()
		if category == taxcalc.CatExemptLongTerm {
			taxableAmount = "0"
		}

		results = append(results, taxcalc.RecordEntry{
			Entry: tax.PluginReportEntry{
				TxID:          rec.TxID,
				RecordType:    "transfer",
				Ts:            rec.Ts,
				Asset:         rec.FeeCurrency,
				Amount:        bucket.Amount.String(),
				PnL:           bucket.PnL.String(),
				HoldingPeriod: bucket.HoldingDays,
				TaxCategory:   category,
				TaxableAmount: taxableAmount,
				CostBasis:     bucket.TotalCostBasis.Add(bucket.TotalFees).String(),
				Proceeds:      bucket.Proceeds.String(),
				Details:       details,
			},
			PnL:     bucket.PnL,
			IsShort: bucket.IsShort,
		})
	}

	return results, nil
}

// processDerivative handles derivative trades (e.g. perpetual futures) using
// dual FIFO position tracking. Derivatives can be opened long (BUY) or short
// (SELL), and PnL is realized only when a position is reduced/closed.
//
// For longs:  BUY opens position → FifoAddLot on ASSET
//
//	SELL closes position → FifoDispose on ASSET, PnL = proceeds - costBasis
//
// For shorts: SELL opens position → FifoAddLot on ASSET:SHORT (costBasis = sell price)
//
//	BUY closes position → FifoDispose on ASSET:SHORT, PnL = costBasis - buyValue
//
// A single trade can partially close one side and open the other if the amount
// exceeds the open position (e.g. flipping from long to short).
// All realized PnL falls under § 20 EStG (Kapitalerträge), no holding period.
func processDerivative(rec tax.PluginRecord) ([]taxcalc.RecordEntry, error) {
	amount, _ := decimal.NewFromString(rec.Amount)
	priceC, _ := decimal.NewFromString(rec.PriceC)
	valueC, _ := decimal.NewFromString(rec.ValueC)
	feeC, _ := decimal.NewFromString(rec.FeeC)

	if amount.IsZero() {
		return nil, nil
	}

	isBuy := rec.Action == 0

	// Use a namespaced asset key so derivative positions (perpetuals, futures)
	// don't share FIFO lots with spot / margin-spot positions.
	// Without this, a derivative short residual (e.g. ETH:SHORT) can be consumed
	// by a margin-spot buy on the same account+asset, draining the spot balance.
	derivAsset := rec.Asset + ":DERIV"
	shortAsset := rec.Asset + ":DERIV:SHORT"

	var results []taxcalc.RecordEntry

	if isBuy {
		// BUY: first try to close open short positions, then open long with remainder.
		disposal, err := tax.FifoDispose(tax.FifoDisposeRequest{
			Account: rec.Account,
			Asset:   shortAsset,
			Ts:      rec.Ts,
			Amount:  amount.String(),
		})
		if err != nil {
			return nil, fmt.Errorf("fifo_dispose (short close) failed: %w", err)
		}

		matchedAmount, _ := decimal.NewFromString(disposal.MatchedAmount)
		unmatchedAmount, _ := decimal.NewFromString(disposal.UnmatchedAmount)

		// Generate PnL for closed short positions.
		// Short lots store the original sell price as costBasis and opening fee as Fee.
		if matchedAmount.GreaterThan(decimal.Zero) {
			closeFee := taxcalc.AllocateFee(feeC, matchedAmount, amount)
			buyPricePerUnit := taxcalc.ComputeProceeds(valueC, amount, priceC).Div(amount)
			buyBackCost := buyPricePerUnit.Mul(matchedAmount)

			result := taxcalc.ComputeDerivativeShortClosePnL(buyBackCost, disposal.Matches, closeFee)

			details := marshalDetails(map[string]any{
				"type":          "derivative_short_close",
				"matches":       disposal.Matches,
				"matchedAmount": disposal.MatchedAmount,
				"buyBackCost":   buyBackCost.String(),
				"isDerivative":  rec.IsDerivative,
				"isMarginTrade": rec.IsMarginTrade,
			})

			results = append(results, taxcalc.RecordEntry{
				Entry: tax.PluginReportEntry{
					TxID:          rec.TxID,
					RecordType:    "trade",
					Ts:            rec.Ts,
					Asset:         rec.Asset,
					Amount:        matchedAmount.String(),
					PnL:           result.PnL.String(),
					HoldingPeriod: 0,
					TaxCategory:   taxcalc.CatKapitalertraege,
					TaxableAmount: result.PnL.String(),
					CostBasis:     result.ReportCostBasis.String(),
					Proceeds:      result.ReportProceeds.String(),
					Details:       details,
				},
				PnL:     result.PnL,
				IsShort: true,
			})
		}

		// Open long position with any unmatched (remaining) amount.
		if unmatchedAmount.GreaterThan(decimal.Zero) {
			perUnitCost := taxcalc.ComputePerUnitCost(valueC, amount, priceC)
			perUnitFee := taxcalc.ComputePerUnitFee(feeC, amount)

			err := tax.FifoAddLot(tax.FifoAddLotRequest{
				Account:   rec.Account,
				Asset:     derivAsset,
				ID:        rec.TxID,
				Ts:        rec.Ts,
				Amount:    unmatchedAmount.String(),
				CostBasis: perUnitCost.String(),
				Fee:       perUnitFee.String(),
				Metadata:  map[string]any{"ticker": rec.Ticker},
			})
			if err != nil {
				return nil, fmt.Errorf("fifo_add_lot (long open) failed: %w", err)
			}
		}
	} else {
		// SELL: first try to close open long positions, then open short with remainder.
		disposal, err := tax.FifoDispose(tax.FifoDisposeRequest{
			Account: rec.Account,
			Asset:   derivAsset,
			Ts:      rec.Ts,
			Amount:  amount.String(),
		})
		if err != nil {
			return nil, fmt.Errorf("fifo_dispose (long close) failed: %w", err)
		}

		matchedAmount, _ := decimal.NewFromString(disposal.MatchedAmount)
		unmatchedAmount, _ := decimal.NewFromString(disposal.UnmatchedAmount)

		// Generate PnL for closed long positions.
		if matchedAmount.GreaterThan(decimal.Zero) {
			closeFee := taxcalc.AllocateFee(feeC, matchedAmount, amount)
			proceeds := taxcalc.ComputeProceeds(valueC, amount, priceC)
			closeProceeds := taxcalc.AllocateProceeds(proceeds, matchedAmount, amount)

			result := taxcalc.ComputeDerivativeLongClosePnL(closeProceeds, disposal.Matches, closeFee)

			details := marshalDetails(map[string]any{
				"type":          "derivative_long_close",
				"matches":       disposal.Matches,
				"matchedAmount": disposal.MatchedAmount,
				"isDerivative":  rec.IsDerivative,
				"isMarginTrade": rec.IsMarginTrade,
			})

			results = append(results, taxcalc.RecordEntry{
				Entry: tax.PluginReportEntry{
					TxID:          rec.TxID,
					RecordType:    "trade",
					Ts:            rec.Ts,
					Asset:         rec.Asset,
					Amount:        matchedAmount.String(),
					PnL:           result.PnL.String(),
					HoldingPeriod: 0,
					TaxCategory:   taxcalc.CatKapitalertraege,
					TaxableAmount: result.PnL.String(),
					CostBasis:     result.ReportCostBasis.String(),
					Proceeds:      result.ReportProceeds.String(),
					Details:       details,
				},
				PnL:     result.PnL,
				IsShort: true,
			})
		}

		// Open short position with any unmatched (remaining) amount.
		// Store the sell price as costBasis so we can compute PnL when closing.
		if unmatchedAmount.GreaterThan(decimal.Zero) {
			perUnitProceeds := taxcalc.ComputeProceeds(valueC, amount, priceC).Div(amount)
			perUnitFee := taxcalc.ComputePerUnitFee(feeC, amount)

			err := tax.FifoAddLot(tax.FifoAddLotRequest{
				Account:   rec.Account,
				Asset:     shortAsset,
				ID:        rec.TxID,
				Ts:        rec.Ts,
				Amount:    unmatchedAmount.String(),
				CostBasis: perUnitProceeds.String(),
				Fee:       perUnitFee.String(),
				Metadata:  map[string]any{"ticker": rec.Ticker},
			})
			if err != nil {
				return nil, fmt.Errorf("fifo_add_lot (short open) failed: %w", err)
			}
		}
	}

	return results, nil
}

// processMarginSpot handles margin spot trades (is_margin_trade=1, is_derivative=0)
// using dual FIFO position tracking, like processDerivative.
//
// Unlike derivatives, margin-spot trades involve actual crypto delivery (borrow → sell)
// and are taxed under § 23 EStG (private Veräußerungsgeschäfte):
//   - 1-year holding period exemption applies
//   - PnL participates in Freigrenze netting
//   - Matches are split into short-term / long-term buckets
func processMarginSpot(rec tax.PluginRecord) ([]taxcalc.RecordEntry, error) {
	amount, _ := decimal.NewFromString(rec.Amount)
	priceC, _ := decimal.NewFromString(rec.PriceC)
	valueC, _ := decimal.NewFromString(rec.ValueC)
	feeC, _ := decimal.NewFromString(rec.FeeC)

	if amount.IsZero() {
		return nil, nil
	}

	isBuy := rec.Action == 0
	shortAsset := rec.Asset + ":SHORT"

	var results []taxcalc.RecordEntry

	if isBuy {
		// BUY: first try to close open short positions, then open long with remainder.
		disposal, err := tax.FifoDispose(tax.FifoDisposeRequest{
			Account: rec.Account,
			Asset:   shortAsset,
			Ts:      rec.Ts,
			Amount:  amount.String(),
		})
		if err != nil {
			return nil, fmt.Errorf("fifo_dispose (margin short close) failed: %w", err)
		}

		matchedAmount, _ := decimal.NewFromString(disposal.MatchedAmount)
		unmatchedAmount, _ := decimal.NewFromString(disposal.UnmatchedAmount)

		// Generate PnL for closed short positions, split by holding period.
		if matchedAmount.GreaterThan(decimal.Zero) {
			closeFee := taxcalc.AllocateFee(feeC, matchedAmount, amount)
			buyPricePerUnit := taxcalc.ComputeProceeds(valueC, amount, priceC).Div(amount)
			buyBackCost := buyPricePerUnit.Mul(matchedAmount)

			shortClose := taxcalc.ComputeShortClosePnLAt(buyBackCost, disposal.Matches, closeFee, rec.Ts)

			details := marshalDetails(map[string]any{
				"type":          "margin_spot_short_close",
				"matches":       disposal.Matches,
				"matchedAmount": disposal.MatchedAmount,
				"buyBackCost":   buyBackCost.String(),
				"isMarginTrade": rec.IsMarginTrade,
			})

			for _, bucket := range []*taxcalc.SaleResult{shortClose.Short, shortClose.Long} {
				if bucket == nil {
					continue
				}

				category := taxcalc.DetermineSaleCategory(bucket.IsShort)

				taxableAmount := bucket.PnL.String()
				if category == taxcalc.CatExemptLongTerm {
					taxableAmount = "0"
				}

				results = append(results, taxcalc.RecordEntry{
					Entry: tax.PluginReportEntry{
						TxID:          rec.TxID,
						RecordType:    "trade",
						Ts:            rec.Ts,
						Asset:         rec.Asset,
						Amount:        bucket.Amount.String(),
						PnL:           bucket.PnL.String(),
						HoldingPeriod: bucket.HoldingDays,
						TaxCategory:   category,
						TaxableAmount: taxableAmount,
						CostBasis:     bucket.TotalCostBasis.Add(bucket.TotalFees).String(),
						Proceeds:      bucket.Proceeds.String(),
						Details:       details,
					},
					PnL:     bucket.PnL,
					IsShort: bucket.IsShort,
				})
			}
		}

		// Open long position with any unmatched (remaining) amount.
		if unmatchedAmount.GreaterThan(decimal.Zero) {
			perUnitCost := taxcalc.ComputePerUnitCost(valueC, amount, priceC)
			perUnitFee := taxcalc.ComputePerUnitFee(feeC, amount)

			err := tax.FifoAddLot(tax.FifoAddLotRequest{
				Account:   rec.Account,
				Asset:     rec.Asset,
				ID:        rec.TxID,
				Ts:        rec.Ts,
				Amount:    unmatchedAmount.String(),
				CostBasis: perUnitCost.String(),
				Fee:       perUnitFee.String(),
				Metadata:  map[string]any{"ticker": rec.Ticker},
			})
			if err != nil {
				return nil, fmt.Errorf("fifo_add_lot (margin long open) failed: %w", err)
			}
		}
	} else {
		// SELL: first try to close open long positions, then open short with remainder.
		disposal, err := tax.FifoDispose(tax.FifoDisposeRequest{
			Account: rec.Account,
			Asset:   rec.Asset,
			Ts:      rec.Ts,
			Amount:  amount.String(),
		})
		if err != nil {
			return nil, fmt.Errorf("fifo_dispose (margin long close) failed: %w", err)
		}

		matchedAmount, _ := decimal.NewFromString(disposal.MatchedAmount)
		unmatchedAmount, _ := decimal.NewFromString(disposal.UnmatchedAmount)

		// Generate PnL for closed long positions — same as normal spot sell.
		if matchedAmount.GreaterThan(decimal.Zero) {
			proceeds := taxcalc.ComputeProceeds(valueC, amount, priceC)
			closeProceeds := taxcalc.AllocateProceeds(proceeds, matchedAmount, amount)
			closeFee := taxcalc.AllocateFee(feeC, matchedAmount, amount)

			sale := taxcalc.ComputeSalePnLAt(closeProceeds, disposal.Matches, closeFee, rec.Ts)

			details := marshalDetails(map[string]any{
				"type":          "margin_spot_long_close",
				"matches":       disposal.Matches,
				"matchedAmount": disposal.MatchedAmount,
				"isMarginTrade": rec.IsMarginTrade,
			})

			for _, bucket := range []*taxcalc.SaleResult{sale.Short, sale.Long} {
				if bucket == nil {
					continue
				}

				category := taxcalc.DetermineSaleCategory(bucket.IsShort)

				taxableAmount := bucket.PnL.String()
				if category == taxcalc.CatExemptLongTerm {
					taxableAmount = "0"
				}

				results = append(results, taxcalc.RecordEntry{
					Entry: tax.PluginReportEntry{
						TxID:          rec.TxID,
						RecordType:    "trade",
						Ts:            rec.Ts,
						Asset:         rec.Asset,
						Amount:        bucket.Amount.String(),
						PnL:           bucket.PnL.String(),
						HoldingPeriod: bucket.HoldingDays,
						TaxCategory:   category,
						TaxableAmount: taxableAmount,
						CostBasis:     bucket.TotalCostBasis.Add(bucket.TotalFees).String(),
						Proceeds:      bucket.Proceeds.String(),
						Details:       details,
					},
					PnL:     bucket.PnL,
					IsShort: bucket.IsShort,
				})
			}
		}

		// Open short position with any unmatched (remaining) amount.
		if unmatchedAmount.GreaterThan(decimal.Zero) {
			perUnitProceeds := taxcalc.ComputeProceeds(valueC, amount, priceC).Div(amount)
			perUnitFee := taxcalc.ComputePerUnitFee(feeC, amount)

			err := tax.FifoAddLot(tax.FifoAddLotRequest{
				Account:   rec.Account,
				Asset:     shortAsset,
				ID:        rec.TxID,
				Ts:        rec.Ts,
				Amount:    unmatchedAmount.String(),
				CostBasis: perUnitProceeds.String(),
				Fee:       perUnitFee.String(),
				Metadata:  map[string]any{"ticker": rec.Ticker},
			})
			if err != nil {
				return nil, fmt.Errorf("fifo_add_lot (margin short open) failed: %w", err)
			}
		}
	}

	return results, nil
}

func marshalDetails(data map[string]any) string {
	b, _ := json.Marshal(data)
	return string(b)
}

// computeSummary aggregates report entries into country-specific summary rows.
func computeSummary(entries []taxcalc.RecordEntry, freigrenze taxcalc.FreigrenzeResult, taxYear int, currency string) []tax.ReportSummaryRow {
	privateVeraeusserung := decimal.Zero
	exemptLongTerm := decimal.Zero
	exemptFreigrenze := decimal.Zero
	sonstigeEinkuenfte := decimal.Zero
	kapitalertraege := decimal.Zero
	transferFees := decimal.Zero
	totalPnL := decimal.Zero

	for _, e := range entries {
		pnl := e.PnL

		switch e.Entry.TaxCategory {
		case taxcalc.CatPrivateVeraeusserung:
			privateVeraeusserung = privateVeraeusserung.Add(pnl)
		case taxcalc.CatExemptLongTerm:
			exemptLongTerm = exemptLongTerm.Add(pnl)
		case taxcalc.CatExemptFreigrenze:
			exemptFreigrenze = exemptFreigrenze.Add(pnl)
		case taxcalc.CatSonstigeEinkuenfte:
			sonstigeEinkuenfte = sonstigeEinkuenfte.Add(pnl)
		case taxcalc.CatKapitalertraege:
			kapitalertraege = kapitalertraege.Add(pnl)
		case taxcalc.CatTransferFee:
			transferFees = transferFees.Add(pnl)
		}

		totalPnL = totalPnL.Add(pnl)
	}

	suffix := " " + currency
	format := func(d decimal.Decimal) string {
		return d.StringFixed(2) + suffix
	}

	rows := []tax.ReportSummaryRow{
		{Label: "Steuerpflichtige private Veräußerungsgeschäfte (§23 EStG)", Value: format(privateVeraeusserung), Order: 1},
		{Label: "Steuerfreie Veräußerungen (Haltefrist > 1 Jahr)", Value: format(exemptLongTerm), Order: 2},
	}

	if freigrenze.ShouldApply {
		rows = append(rows, tax.ReportSummaryRow{
			Label: fmt.Sprintf("Steuerfreie Veräußerungen (Freigrenze %s)", taxcalc.FreigrenzeForYear(taxYear).String()+suffix),
			Value: format(exemptFreigrenze),
			Order: 3,
		})
	}

	rows = append(rows,
		tax.ReportSummaryRow{Label: "Transfergebühren", Value: format(transferFees), Order: 4},
		tax.ReportSummaryRow{Label: "Kapitalerträge (§20 EStG)", Value: format(kapitalertraege), Order: 5},
		tax.ReportSummaryRow{Label: "Sonstige Einkünfte (§22 Nr. 3 EStG)", Value: format(sonstigeEinkuenfte), Order: 6},
		tax.ReportSummaryRow{Label: "Gesamt realisierter Gewinn/Verlust", Value: format(totalPnL), Order: 7},
	)

	if freigrenze.NetShortTermPnL.IsNegative() {
		rows = append(rows, tax.ReportSummaryRow{
			Label: "Verlustvortrag (§23 Abs. 3 Satz 8 EStG)",
			Value: format(freigrenze.NetShortTermPnL),
			Order: 8,
		})
	}

	return rows
}

func computeMonthlySummaries(entries []taxcalc.RecordEntry, freigrenze taxcalc.FreigrenzeResult, taxYear int, currency string) []tax.ReportMonthlySummary {
	grouped := map[string][]taxcalc.RecordEntry{}
	monthTitles := map[string]string{}

	for _, entry := range entries {
		entryTs, err := time.Parse(time.RFC3339Nano, entry.Entry.Ts)
		if err != nil {
			entryTs, err = time.Parse(time.RFC3339, entry.Entry.Ts)
			if err != nil {
				continue
			}
		}

		monthStart := time.Date(entryTs.Year(), entryTs.Month(), 1, 0, 0, 0, 0, time.UTC)
		monthKey := monthStart.Format("2006-01")
		grouped[monthKey] = append(grouped[monthKey], entry)
		monthTitles[monthKey] = monthStart.Format("January 2006")
	}

	monthKeys := make([]string, 0, len(grouped))
	for monthKey := range grouped {
		monthKeys = append(monthKeys, monthKey)
	}
	sort.Strings(monthKeys)

	result := make([]tax.ReportMonthlySummary, 0, len(monthKeys))
	for _, monthKey := range monthKeys {
		result = append(result, tax.ReportMonthlySummary{
			Month: monthKey,
			Title: monthTitles[monthKey],
			Rows:  computeMonthlySummaryRows(grouped[monthKey], freigrenze, taxYear, currency),
		})
	}

	return result
}

func computeMonthlySummaryRows(entries []taxcalc.RecordEntry, freigrenze taxcalc.FreigrenzeResult, taxYear int, currency string) []tax.ReportSummaryRow {
	privateVeraeusserung := decimal.Zero
	exemptLongTerm := decimal.Zero
	exemptFreigrenze := decimal.Zero
	sonstigeEinkuenfte := decimal.Zero
	kapitalertraege := decimal.Zero
	transferFees := decimal.Zero
	totalPnL := decimal.Zero

	for _, e := range entries {
		pnl := e.PnL

		switch e.Entry.TaxCategory {
		case taxcalc.CatPrivateVeraeusserung:
			privateVeraeusserung = privateVeraeusserung.Add(pnl)
		case taxcalc.CatExemptLongTerm:
			exemptLongTerm = exemptLongTerm.Add(pnl)
		case taxcalc.CatExemptFreigrenze:
			exemptFreigrenze = exemptFreigrenze.Add(pnl)
		case taxcalc.CatSonstigeEinkuenfte:
			sonstigeEinkuenfte = sonstigeEinkuenfte.Add(pnl)
		case taxcalc.CatKapitalertraege:
			kapitalertraege = kapitalertraege.Add(pnl)
		case taxcalc.CatTransferFee:
			transferFees = transferFees.Add(pnl)
		}

		totalPnL = totalPnL.Add(pnl)
	}

	suffix := " " + currency
	format := func(d decimal.Decimal) string {
		return d.StringFixed(2) + suffix
	}

	rows := []tax.ReportSummaryRow{
		{Label: "Steuerpflichtige private Veräußerungsgeschäfte (§23 EStG)", Value: format(privateVeraeusserung), Order: 1},
		{Label: "Steuerfreie Veräußerungen (Haltefrist > 1 Jahr)", Value: format(exemptLongTerm), Order: 2},
	}

	if freigrenze.ShouldApply {
		rows = append(rows, tax.ReportSummaryRow{
			Label: fmt.Sprintf("Steuerfreie Veräußerungen (Freigrenze %s)", taxcalc.FreigrenzeForYear(taxYear).String()+suffix),
			Value: format(exemptFreigrenze),
			Order: 3,
		})
	}

	rows = append(rows,
		tax.ReportSummaryRow{Label: "Transfergebühren", Value: format(transferFees), Order: 4},
		tax.ReportSummaryRow{Label: "Kapitalerträge (§20 EStG)", Value: format(kapitalertraege), Order: 5},
		tax.ReportSummaryRow{Label: "Sonstige Einkünfte (§22 Nr. 3 EStG)", Value: format(sonstigeEinkuenfte), Order: 6},
		tax.ReportSummaryRow{Label: "Gesamt realisierter Gewinn/Verlust", Value: format(totalPnL), Order: 7},
	)

	return rows
}
