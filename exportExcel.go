package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/extism/go-pdk"
	"github.com/plusev-terminal/go-plugin-common/plugin"
	"github.com/plusev-terminal/go-plugin-common/tax"
	"github.com/xuri/excelize/v2"
)

//go:wasmexport export_entries_excel
func export_entries_excel() int32 {
	var input tax.ExportEntriesInput
	if err := json.Unmarshal(pdk.Input(), &input); err != nil {
		return plugin.WriteResponse(plugin.ErrorResponse(err))
	}

	xlsxBytes, err := buildGermanExcel(input)
	if err != nil {
		return plugin.WriteResponse(plugin.ErrorResponse(err))
	}

	pdk.Output(xlsxBytes)
	return 0
}

func buildGermanExcel(input tax.ExportEntriesInput) ([]byte, error) {
	workbook := excelize.NewFile()
	defer workbook.Close()

	sheetName := "Einträge"
	workbook.SetSheetName(workbook.GetSheetName(0), sheetName)

	headers := []string{
		"Datum",
		"Tx-ID",
		"Typ",
		"Asset",
		"Kategorie",
		"Menge",
		"Anschaffungskosten/Einheit",
		"Anschaffungskosten",
		"Veräußerungserlös",
		"Gewinn / Verlust",
		"Steuerpflichtiger Betrag",
		"Haltedauer (Tage)",
	}

	for colNum, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(colNum+1, 1)
		workbook.SetCellValue(sheetName, cell, header)
	}

	headStyle, err := workbook.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err == nil {
		lastHeaderCell, _ := excelize.CoordinatesToCellName(len(headers), 1)
		workbook.SetCellStyle(sheetName, "A1", lastHeaderCell, headStyle)
	}

	// Excel custom number formats always use , for thousands and . for decimal point.
	// The display locale handles rendering with comma as decimal separator.
	deDecimalFmt := "#,##0.################"
	deDecimalStyle, _ := workbook.NewStyle(&excelize.Style{CustomNumFmt: &deDecimalFmt})

	deCurrencyFmt := fmt.Sprintf(`#,##0.00 "%s"`, input.Currency)
	deCurrencyStyle, _ := workbook.NewStyle(&excelize.Style{CustomNumFmt: &deCurrencyFmt})

	integerFmt := "0"
	integerStyle, _ := workbook.NewStyle(&excelize.Style{CustomNumFmt: &integerFmt})

	for rowNum, entry := range input.Entries {
		excelRow := rowNum + 2
		costBasisUnit := 0.0
		if entry.Amount != 0 {
			costBasisUnit = entry.CostBasis / entry.Amount
		}

		ts, _ := time.Parse(time.RFC3339, entry.Ts)
		dateStr := ts.Format("02.01.2006 15:04:05")

		cells := []struct {
			col   int
			value any
		}{
			{col: 1, value: dateStr},
			{col: 2, value: entry.TxID},
			{col: 3, value: entry.RecordType},
			{col: 4, value: entry.Asset},
			{col: 5, value: entry.TaxCategory},
			{col: 6, value: entry.Amount},
			{col: 7, value: costBasisUnit},
			{col: 8, value: entry.CostBasis},
			{col: 9, value: entry.Proceeds},
			{col: 10, value: entry.PnL},
			{col: 11, value: entry.TaxableAmount},
			{col: 12, value: entry.HoldingPeriod},
		}

		for _, item := range cells {
			cell, _ := excelize.CoordinatesToCellName(item.col, excelRow)
			workbook.SetCellValue(sheetName, cell, item.value)
		}
	}

	lastRow := len(input.Entries) + 1
	lastRowStr := fmt.Sprintf("%d", lastRow)

	// Apply styles: Amount column uses decimal, currency columns use currency format.
	if deDecimalStyle != 0 {
		workbook.SetCellStyle(sheetName, "F2", "F"+lastRowStr, deDecimalStyle)
	}
	if deCurrencyStyle != 0 {
		workbook.SetCellStyle(sheetName, "G2", "K"+lastRowStr, deCurrencyStyle)
	}
	if integerStyle != 0 {
		workbook.SetCellStyle(sheetName, "L2", "L"+lastRowStr, integerStyle)
	}

	workbook.SetColWidth(sheetName, "A", "A", 20)
	workbook.SetColWidth(sheetName, "B", "B", 72)
	workbook.SetColWidth(sheetName, "C", "E", 16)
	workbook.SetColWidth(sheetName, "F", "F", 22)
	workbook.SetColWidth(sheetName, "G", "K", 26)
	workbook.SetColWidth(sheetName, "L", "L", 18)

	workbook.SetPanes(sheetName, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})

	buf, err := workbook.WriteToBuffer()
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
