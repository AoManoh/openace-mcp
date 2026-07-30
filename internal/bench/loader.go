package bench

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// FileSHA256 计算文件内容摘要（run 记录与封存台账用）。
func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// LoadQueries 读取统一 queries.jsonl。
func LoadQueries(path string) ([]Query, error) {
	var out []Query
	err := forEachLine(path, func(line []byte) error {
		var query Query
		if err := json.Unmarshal(line, &query); err != nil {
			return err
		}
		if query.ID == "" {
			return fmt.Errorf("query 缺 id")
		}
		out = append(out, query)
		return nil
	})
	return out, err
}

// LoadDocs 流式读取统一 corpus.jsonl，逐条回调（大语料不驻留）。
func LoadDocs(path string, visit func(Doc) error) error {
	return forEachLine(path, func(line []byte) error {
		var doc Doc
		if err := json.Unmarshal(line, &doc); err != nil {
			return err
		}
		if doc.ID == "" {
			return fmt.Errorf("doc 缺 id")
		}
		return visit(doc)
	})
}

// LoadQrels 读取 TSV（qid \t docid \t rel）。
func LoadQrels(path string) (Qrels, error) {
	qrels := Qrels{}
	err := forEachLine(path, func(line []byte) error {
		fields := strings.Split(string(line), "\t")
		if len(fields) < 3 {
			return fmt.Errorf("qrels 行字段不足: %q", line)
		}
		rel, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil {
			return fmt.Errorf("qrels rel 非整数: %q", line)
		}
		qid, docid := fields[0], fields[1]
		if qrels[qid] == nil {
			qrels[qid] = map[string]int{}
		}
		qrels[qid][docid] = rel
		return nil
	})
	return qrels, err
}

func forEachLine(path string, visit func([]byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if err := visit(line); err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
	}
	return scanner.Err()
}
