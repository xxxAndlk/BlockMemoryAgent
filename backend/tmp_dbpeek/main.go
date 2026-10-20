// 临时调试工具：查看指定会话的 session_history 与最近事件，排查上下文组装问题。
package main

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

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

	if len(os.Args) < 2 {
		rows, err := db.Query(`SELECT session_id, work_dir, status, created_at, left(coalesce(summary,''),100)
			FROM session_history WHERE work_dir ILIKE '%moyingshanhai%' ORDER BY created_at DESC LIMIT 10`)
		if err != nil {
			panic(err)
		}
		defer rows.Close()
		for rows.Next() {
			var sid, wd, st, ca, sum string
			if err := rows.Scan(&sid, &wd, &st, &ca, &sum); err != nil {
				panic(err)
			}
			fmt.Printf("%s | %s | %s | %s | %s\n", sid, wd, st, ca, sum)
		}
		return
	}
	sid := os.Args[1]

	// 事件类型统计
	rows, err := db.Query(`SELECT type, kind, count(*) FROM session_events WHERE session_id=$1 GROUP BY 1,2 ORDER BY 3 DESC`, sid)
	if err != nil {
		panic(err)
	}
	fmt.Println("== event type/kind counts ==")
	for rows.Next() {
		var t, k string
		var c int
		if err := rows.Scan(&t, &k, &c); err != nil {
			panic(err)
		}
		fmt.Printf("%-20s %-16s %d\n", t, k, c)
	}
	rows.Close()

	// 全部用户消息时间线
	ur, err := db.Query(`SELECT id, timestamp, kind, left(message,200) FROM session_events
		WHERE session_id=$1 AND type='user_message' ORDER BY id ASC`, sid)
	if err != nil {
		panic(err)
	}
	fmt.Println("\n== user messages ==")
	for ur.Next() {
		var id int
		var ts, k, m string
		if err := ur.Scan(&id, &ts, &k, &m); err != nil {
			panic(err)
		}
		fmt.Printf("[%d] %s kind=%s | %s\n", id, ts, k, m)
	}
	ur.Close()

	// 哪些事件带 prompt 列
	if len(os.Args) >= 3 && os.Args[2] == "prompts" {
		pr, err := db.Query(`SELECT id, timestamp, type, kind, agent, input_tokens, length(prompt)
			FROM session_events WHERE session_id=$1 AND prompt <> '' ORDER BY id ASC`, sid)
		if err != nil {
			panic(err)
		}
		defer pr.Close()
		fmt.Println("== events with prompt ==")
		for pr.Next() {
			var id, it, plen int
			var ts, t, k, ag string
			if err := pr.Scan(&id, &ts, &t, &k, &ag, &it, &plen); err != nil {
				panic(err)
			}
			fmt.Printf("[%d] %s %-10s %-14s %-12s tokens=%-7d len=%d\n", id, ts, t, k, ag, it, plen)
		}
		return
	}

	// dump 指定事件之后的 prompt 尾部：看模型实际收到的上下文结尾
	if len(os.Args) >= 4 && os.Args[3] == "tail" {
		after := os.Args[2]
		pr, err := db.Query(`SELECT id, timestamp, type, kind, input_tokens, length(prompt), left(prompt,600), right(prompt,2500)
			FROM session_events WHERE session_id=$1 AND id >= $2::bigint AND prompt <> '' ORDER BY id ASC`, sid, after)
		if err != nil {
			panic(err)
		}
		defer pr.Close()
		for pr.Next() {
			var id, it, plen int
			var ts, t, k, head, tail string
			if err := pr.Scan(&id, &ts, &t, &k, &it, &plen, &head, &tail); err != nil {
				panic(err)
			}
			fmt.Printf("\n#### [%d] %s %s/%s tokens=%d promptLen=%d\n--- head ---\n%s\n--- tail ---\n%s\n", id, ts, t, k, it, plen, head, tail)
		}
		return
	}

	// 找包含关键词的用户消息
	if len(os.Args) >= 3 {
		kw := os.Args[2]
		qr, err := db.Query(`SELECT id, timestamp, type, kind, left(message,200) FROM session_events
			WHERE session_id=$1 AND message ILIKE '%'||$2||'%' ORDER BY id ASC LIMIT 30`, sid, kw)
		if err != nil {
			panic(err)
		}
		fmt.Printf("\n== events containing %q ==\n", kw)
		for qr.Next() {
			var id int
			var ts, t, k, m string
			if err := qr.Scan(&id, &ts, &t, &k, &m); err != nil {
				panic(err)
			}
			fmt.Printf("[%d] %s %-10s %-12s | %s\n", id, ts, t, k, m)
		}
		qr.Close()
	}
	_ = sort.Strings
	_ = strings.Contains
}
