// 临时调试工具2：dump session_logs 里该会话最后几次 LLM 调用的 prompt 尾部。
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
	n := 6
	if len(os.Args) >= 3 {
		fmt.Sscanf(os.Args[2], "%d", &n)
	}
	rows, err := db.Query(`SELECT id, created_at, agent, model, input_tokens, output_tokens, length(coalesce(prompt,'')), right(coalesce(prompt,''),3000)
		FROM session_logs WHERE session_id=$1 AND prompt IS NOT NULL AND prompt <> '' ORDER BY id DESC LIMIT $2`, sid, n)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	type rec struct {
		id, it, ot, plen int
		ca, ag, m, tail  string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.ca, &r.ag, &r.m, &r.it, &r.ot, &r.plen, &r.tail); err != nil {
			panic(err)
		}
		recs = append(recs, r)
	}
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		fmt.Printf("\n########## log#%d %s agent=%s model=%s in=%d out=%d promptLen=%d\n", r.id, r.ca, r.ag, r.m, r.it, r.ot, r.plen)
		fmt.Println(r.tail)
	}
}
