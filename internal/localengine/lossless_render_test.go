package localengine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// 结果组装只删除内容相同的重复文本：同一行号范围里内容不同的字节片段
// 各自保留；同分时的输出次序在重复查询之间保持一致。

// fragmentLine 生成一行：每段以 prefix 开头，用 x 填满到 segmentBytes 字节。
func fragmentLine(markers []string, segmentBytes int) string {
	var body strings.Builder
	for _, marker := range markers {
		prefix := "sharedmarker " + marker + " "
		body.WriteString(prefix + strings.Repeat("x", segmentBytes-len(prefix)-1) + " ")
	}
	return body.String()
}

func assertMarkers(t *testing.T, text string, markers []string) {
	t.Helper()
	for _, marker := range markers {
		if strings.Count(text, marker) != 1 {
			t.Fatalf("标记 %s 应恰好出现一次，实际 %d 次", marker, strings.Count(text, marker))
		}
	}
}

// TestLongLineFragmentsAllRendered：6144 字节单行切成三个字节片段，三段行号
// 都是 1-1、内容不同，查询命中全部三段。full 与 paths 两种详略都保留三块，
// 头行格式不变，展示计数按实际块数。
func TestLongLineFragmentsAllRendered(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	markers := []string{"segmentOne", "segmentTwo", "segmentThree"}
	writeFixture(t, root, "singleline.txt", fragmentLine(markers, 2048))
	req := searchRequest(root, "sharedmarker")
	res, err := e.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertMarkers(t, res.Text, markers)
	if len(res.Hits) != 3 || res.Display == nil || res.Display.CandidateBlocks != 3 {
		t.Fatalf("应保留三块: hits=%+v display=%+v", res.Hits, res.Display)
	}
	for _, hit := range res.Hits {
		if hit.StartLine != 1 || hit.EndLine != 1 {
			t.Fatalf("三块都应是第 1 行: %+v", res.Hits)
		}
	}
	if strings.Count(res.Text, "## singleline.txt:1-1") != 3 {
		t.Fatalf("头行格式应保持 path:start-end 且有三个: %q", res.Text)
	}
	req.Detail = "paths"
	paths, err := e.Search(context.Background(), req)
	if err != nil || len(paths.Hits) != 3 {
		t.Fatalf("paths 详略也应列出三块: hits=%+v err=%v", paths.Hits, err)
	}
}

// TestCrossLineFragmentsKeepDistinctText：两行各约 3000 字节，字节窗口跨行：
// 第二个片段从第 1 行中间开始、到第 2 行中间结束。相交行号上的正文不同，
// 不能按行号裁掉后一个片段的开头或整段丢弃。
func TestCrossLineFragmentsKeepDistinctText(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	line1 := "sharedmarker fragAlpha " + strings.Repeat("a", 2100-len("sharedmarker fragAlpha ")) +
		" sharedmarker fragBeta " + strings.Repeat("b", 800)
	line2 := strings.Repeat("c", 900) + " " + strings.Repeat("d", 1500) + " sharedmarker fragGamma " + strings.Repeat("e", 600)
	writeFixture(t, root, "twolines.txt", line1+"\n"+line2+"\n")
	res, err := e.Search(context.Background(), searchRequest(root, "sharedmarker"))
	if err != nil {
		t.Fatal(err)
	}
	assertMarkers(t, res.Text, []string{"fragAlpha", "fragBeta", "fragGamma"})
}

// TestMultibyteLongLineFragmentsValidUTF8：中文长行按 UTF-8 边界切片，
// 渲染后正文仍是合法 UTF-8，三个标记都在。
func TestMultibyteLongLineFragmentsValidUTF8(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	var body strings.Builder
	for _, marker := range []string{"markerUno", "markerDos", "markerTres"} {
		body.WriteString("sharedmarker " + marker + " " + strings.Repeat("中文内容", 160) + " ")
	}
	writeFixture(t, root, "cjk.txt", body.String())
	res, err := e.Search(context.Background(), searchRequest(root, "sharedmarker"))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(res.Text) {
		t.Fatal("渲染正文应是合法 UTF-8")
	}
	assertMarkers(t, res.Text, []string{"markerUno", "markerDos", "markerTres"})
}

// TestRerankedLongLineFragmentsAllRendered：精排把顺序反过来之后组装，三段
// 仍全部保留。
func TestRerankedLongLineFragmentsAllRendered(t *testing.T) {
	ts := newRerankServer(t, func(doc string) float64 {
		switch {
		case strings.Contains(doc, "segmentThree"):
			return 0.9
		case strings.Contains(doc, "segmentTwo"):
			return 0.5
		}
		return 0.1
	}, nil)
	e := newTestEngineWith(t, Options{Rerank: rerankOptions(ts.URL)})
	root := t.TempDir()
	markers := []string{"segmentOne", "segmentTwo", "segmentThree"}
	writeFixture(t, root, "singleline.txt", fragmentLine(markers, 2048))
	res, err := e.Search(context.Background(), searchRequest(root, "sharedmarker"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.RetrievalMode, "rerank") || len(res.Hits) != 3 {
		t.Fatalf("精排后应保留三块: mode=%q hits=%+v", res.RetrievalMode, res.Hits)
	}
	assertMarkers(t, res.Text, markers)
	if strings.Index(res.Text, "segmentThree") > strings.Index(res.Text, "segmentOne") {
		t.Fatalf("精排分最高的 segmentThree 应排在前面: %q", res.Text)
	}
}

// TestEqualScoreOrderStableAcrossQueries：十个内容相同的文件同分，同一查询
// 连续 40 次的结果次序完全一致。
func TestEqualScoreOrderStableAcrossQueries(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		writeFixture(t, root, fmt.Sprintf("item%02d.txt", i), "sharedtoken common content.\n")
	}
	orders := map[string]int{}
	for i := 0; i < 40; i++ {
		res, err := e.Search(context.Background(), searchRequest(root, "sharedtoken"))
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, h := range res.Hits {
			paths = append(paths, h.Path)
		}
		orders[strings.Join(paths, ",")]++
	}
	if len(orders) != 1 {
		t.Fatalf("40 次相同查询应只有一种次序，实际 %d 种", len(orders))
	}
	for order := range orders {
		if strings.Count(order, ",") != 9 {
			t.Fatalf("应返回 10 个文件: %s", order)
		}
	}
}
