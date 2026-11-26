package db

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var (
	scoreLotCaption    = regexp.MustCompile(`^(.*?)(?:综合评分明细表|详细评分表|评分明细表|详细评审表|评分标准表)[：:]?$`)
	scoreCaptionMarker = regexp.MustCompile(`(?:第)?([A-Z]|\d{1,2}|[一二三四五六七八九十]{1,3})(?:标段|分标|分包|包)|(?:标段|分标|分包|包)([A-Z]|\d{1,2}|[一二三四五六七八九十]{1,3})`)
	tenderLotHeading   = regexp.MustCompile(`^(?:合同包([A-Z]|\d{1,2})[（(]|标项([A-Z]|\d{1,2}|[一二三四五六七八九十]{1,3})[：:]?$)`)
	tenderPackageCover = regexp.MustCompile(`^项目编号[-－]包号[:：]\s*[^\s:：]+-([A-Za-z]|\d{1,2})$`)
)

func appendScoringLot(lots []string, lot string) []string {
	lot = normalizeLot(lot)
	if n, err := strconv.Atoi(lot); err == nil && n > 0 {
		lot = strconv.Itoa(n)
	}
	for _, prior := range lots {
		if prior == lot {
			return lots
		}
	}
	if lot != "" {
		return append(lots, lot)
	}
	return lots
}

func lotsFromScoringCaption(text string) []string {
	var lots []string
	for _, line := range strings.Split(text, "\n") {
		m := scoreLotCaption.FindStringSubmatch(normCell(line))
		if m == nil {
			continue
		}
		prefix := strings.Trim(m[1], "（()）")
		prefix = reNumbering.ReplaceAllString(prefix, "")
		markers := scoreCaptionMarker.FindAllStringSubmatch(prefix, -1)
		rest := scoreCaptionMarker.ReplaceAllString(prefix, "")
		if len(markers) == 0 || strings.Trim(rest, "和与、，,/及") != "" {
			continue
		}
		lots = nil
		for _, marker := range markers {
			lots = appendScoringLot(lots, marker[1]+marker[2])
		}
	}
	return lots
}

type documentScoreLots struct {
	cover string
	all   []string
}

// Resolve packages from document metadata, a package-specific cover, or the
// package declarations in the announcement. Numbers in criteria are not
// tender package declarations.
func readDocumentScoreLots(order []*chunkSource) map[int64]*documentScoreLots {
	out := map[int64]*documentScoreLots{}
	for _, cs := range order {
		doc := out[cs.DocID]
		if doc == nil {
			doc = &documentScoreLots{}
			out[cs.DocID] = doc
			if lot, _ := cs.Loc["lot_no"].(string); lot != "" {
				if lots := normalizeLots([]string{lot}); len(lots) == 1 {
					doc.cover = lots[0]
				}
			}
		}
		page, _ := cs.Loc["page_start"].(int)
		seq, _ := cs.Loc["seq"].(int)
		if doc.cover != "" || page > 12 || seq > 80 {
			continue
		}
		for _, line := range strings.Split(cs.Content, "\n") {
			line = normCell(line)
			if page <= 2 && seq <= 6 {
				if m := tenderPackageCover.FindStringSubmatch(line); m != nil {
					doc.cover = appendScoringLot(nil, m[1])[0]
					break
				}
			}
			if m := tenderLotHeading.FindStringSubmatch(line); m != nil {
				doc.all = appendScoringLot(doc.all, m[1]+m[2])
			}
		}
		if doc.cover == "" && (strings.Contains(cs.Content, "包名称") || strings.Contains(cs.Content, "分标号")) {
			doc.all = packageTableScoreLots(cs.Tables, doc.all)
		}
	}
	return out
}

func packageTableScoreLots(raw []byte, lots []string) []string {
	var tables []scoreTable
	if json.Unmarshal(raw, &tables) != nil {
		return lots
	}
	for _, table := range tables {
		number, name := -1, -1
		for i, cell := range firstRow(table.Grid) {
			switch normCell(cell) {
			case "分标号":
				number = i
			case "包名称", "分标名称":
				name = i
			}
		}
		if name < 0 {
			continue
		}
		for _, row := range table.Grid[1:] {
			if name >= len(row) {
				continue
			}
			if number >= 0 && number < len(row) && normalizeLot(row[number]) != "" {
				v := normCell(row[number])
				if _, err := strconv.Atoi(v); err == nil {
					lots = appendScoringLot(lots, v)
					continue
				}
			}
			for _, m := range scoreCaptionMarker.FindAllStringSubmatch(normCell(row[name]), -1) {
				lots = appendScoringLot(lots, m[1]+m[2])
			}
		}
	}
	return lots
}
