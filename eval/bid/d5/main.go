// Command d5 rehearses paired blind-review scoring with fictional data.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	fixtures := filepath.Join("eval", "bid", "fixtures", "d5")
	planPath := flag.String("plan", filepath.Join(fixtures, "plan.json"), "评审方案和匿名稿件编号")
	scoresPath := flag.String("scores", filepath.Join(fixtures, "scores.csv"), "模拟评审分数")
	keyPath := flag.String("key", filepath.Join(fixtures, "key.json"), "揭盲映射")
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}

	result, err := rehearse(*planPath, *scoresPath, *keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rehearse(planPath, scoresPath, keyPath string) (report, error) {
	plan, planHash, err := loadJSON[reviewPlan](planPath)
	if err != nil {
		return report{}, err
	}
	drafts, err := validatePlan(plan)
	if err != nil {
		return report{}, fmt.Errorf("评审方案: %w", err)
	}
	scoreData, err := os.ReadFile(scoresPath)
	if err != nil {
		return report{}, err
	}
	panel, err := readPanel(bytes.NewReader(scoreData), plan, drafts)
	if err != nil {
		return report{}, fmt.Errorf("评审分数: %w", err)
	}
	key, keyHash, err := loadJSON[unblindKey](keyPath)
	if err != nil {
		return report{}, err
	}
	if err := validateKey(plan, key); err != nil {
		return report{}, fmt.Errorf("揭盲映射: %w", err)
	}
	result := summarize(plan, panel, key)
	result.InputSHA256 = inputHashes{
		Plan:   planHash,
		Scores: fmt.Sprintf("%x", sha256.Sum256(scoreData)),
		Key:    keyHash,
	}
	return result, nil
}

func loadJSON[T any](path string) (T, string, error) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil {
		return value, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, "", fmt.Errorf("%s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return value, "", fmt.Errorf("%s: 需要单个 JSON 对象", path)
	}
	return value, fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
