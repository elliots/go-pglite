// Command pglite-repl provides a simple REPL for interacting with an embedded
// PGlite PostgreSQL instance.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	pglite "github.com/elliots/go-pglite"
)

func main() {
	fmt.Println("go-pglite REPL")
	fmt.Println("Initializing PostgreSQL (this may take a moment)...")

	pg, err := pglite.New(pglite.Config{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start PGlite: %v\n", err)
		os.Exit(1)
	}
	defer pg.Close()

	db := pg.DB()

	fmt.Println("PostgreSQL ready.")
	fmt.Println("Type SQL queries, or 'quit' to exit.")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("pglite> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "quit" || line == "\\q" {
			break
		}

		rows, err := db.Query(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}

		cols, _ := rows.Columns()
		if len(cols) > 0 {
			fmt.Println(strings.Join(cols, " | "))
			fmt.Println(strings.Repeat("-", len(strings.Join(cols, " | "))))
		}

		count := 0
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				fmt.Fprintf(os.Stderr, "Error reading row: %v\n", err)
				break
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				switch b := v.(type) {
				case []byte:
					parts[i] = string(b)
				default:
					parts[i] = fmt.Sprintf("%v", v)
				}
			}
			fmt.Println(strings.Join(parts, " | "))
			count++
		}
		if err := rows.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		} else if len(cols) > 0 {
			fmt.Printf("(%d rows)\n", count)
		}
		rows.Close()
	}
}
