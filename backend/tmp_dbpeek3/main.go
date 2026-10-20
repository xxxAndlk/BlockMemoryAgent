// 临时调试工具3：列出该会话某时间点之后的 session_logs 概览 + 指定日志的 prompt 尾部。
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/lib/pq"
)

func main() {
	dsn := os.Getenv("PEEK_DSN")
	if dsn == "" {
		dsn = "postgres://blockmemory:blockmemory_dev@localhost:5432/blockmemory?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	sid := os.Args[1]
	mode := os.Args[2]

	switch mode {
	case "after":
		// after <ts> ：列出该时间之后的所有 llm 调用概览
		ts := os.Args[3]
		rows, err := db.Query(`SELECT id, created_at, agent, model, input_tokens, output_tokens, length(coalesce(prompt,'')), left(coalesce(response,''),150)
			FROM session_logs WHERE session_id=$1 AND prompt IS NOT NULL AND prompt <> '' AND created_at >= $2::timestamp ORDER BY id ASC`, sid, ts)
		if err != nil {
			panic(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, it, ot, plen int
			var ca, ag, m, resp string
			if err := rows.Scan(&id, &ca, &ag, &m, &it, &ot, &plen, &resp); err != nil {
				panic(err)
			}
			fmt.Printf("log#%-6d %s %-16s %-22s in=%-7d out=%-6d len=%-8d resp=%q\n", id, ca, ag, m, it, ot, plen, resp)
		}
	case "tail":
		// tail <logid> <n>：dump 指定日志 prompt 的最后 n 字符
		var logID, n int
		fmt.Sscanf(os.Args[3], "%d", &logID)
		n = 3000
		if len(os.Args) >= 5 {
			fmt.Sscanf(os.Args[4], "%d", &n)
		}
		var prompt string
		if err := db.QueryRow(`SELECT prompt FROM session_logs WHERE id=$1`, logID).Scan(&prompt); err != nil {
			panic(err)
		}
		if len(prompt) > n {
			prompt = prompt[len(prompt)-n:]
		}
		fmt.Println(prompt)
	case "msgs":
		// msgs <logid>：解析 prompt JSON，列出 messages 数组每条的角色与前 80 字（重点看末尾几条）
		var logID int
		fmt.Sscanf(os.Args[3], "%d", &logID)
		var prompt string
		if err := db.QueryRow(`SELECT prompt FROM session_logs WHERE id=$1`, logID).Scan(&prompt); err != nil {
			panic(err)
		}
		var req struct {
			System   string `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := jsonUnmarshal([]byte(prompt), &req); err != nil {
			fmt.Println("parse err:", err)
			fmt.Println(prompt[:min(500, len(prompt))])
			return
		}
		fmt.Printf("system len=%d\n", len(req.System))
		total := len(req.Messages)
		for i, m := range req.Messages {
			c := m.Content
			if len(c) > 100 {
				c = c[:100]
			}
			c = stringsReplaceAll(c)
			fmt.Printf("[%3d] %-10s %s\n", i, m.Role, c)
		}
		_ = total
	case "syscheck":
		// syscheck <logid> <kw>：在 prompt JSON 的 system 字段里搜关键词，输出命中上下文
		var logID int
		fmt.Sscanf(os.Args[3], "%d", &logID)
		var prompt string
		if err := db.QueryRow(`SELECT prompt FROM session_logs WHERE id=$1`, logID).Scan(&prompt); err != nil {
			panic(err)
		}
		var req struct {
			System string `json:"system"`
		}
		if err := jsonUnmarshal([]byte(prompt), &req); err != nil {
			panic(err)
		}
		kw := os.Args[4]
		idx := 0
		for {
			i := stringsIndex(req.System[idx:], kw)
			if i < 0 {
				break
			}
			s := idx + i
			lo := s - 300
			if lo < 0 {
				lo = 0
			}
			hi := s + 500
			if hi > len(req.System) {
				hi = len(req.System)
			}
			fmt.Printf("--- hit @%d ---\n%s\n", s, req.System[lo:hi])
			idx = s + len(kw)
		}
		if idx == 0 {
			fmt.Println("NOT FOUND in system:", kw)
		}
		// 同时在整个 prompt 里统计关键词出现次数
		fmt.Printf("\ncount in full prompt: %d\n", stringsCount(prompt, kw))
	case "events":
		er, err := db.Query(`SELECT id, timestamp, type, kind, agent, left(message,180) FROM session_events
			WHERE session_id=$1 AND id >= 949160 ORDER BY id ASC`, sid)
		if err != nil {
			panic(err)
		}
		defer er.Close()
		for er.Next() {
			var id int
			var ts2, t, k, ag, m string
			if err := er.Scan(&id, &ts2, &t, &k, &ag, &m); err != nil {
				panic(err)
			}
			fmt.Printf("[%d] %s %-12s %-16s %-14s | %s\n", id, ts2, t, k, ag, m)
		}
	}
}
