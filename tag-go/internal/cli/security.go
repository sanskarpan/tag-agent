package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/security"
)

func registerSecurity(root *cobra.Command, app *App) {
	var maxFiles int
	s := &cobra.Command{Use: "security", Short: "Secret scanning & security auditing", GroupID: "tools",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		}}
	scan := &cobra.Command{Use: "scan [PATH]", Short: "Scan a path for secrets", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := "."
			if len(args) == 1 {
				p = args[0]
			}
			abs, _ := filepath.Abs(p)
			if _, err := os.Stat(abs); err != nil {
				return fmt.Errorf("path not found: %s", p)
			}
			st, _ := os.Stat(abs)
			var findings []security.Finding
			if st.IsDir() {
				findings = security.ScanDir(abs, maxFiles)
			} else {
				findings = security.ScanFile(abs)
			}
			db, err := app.OpenDB()
			if err == nil {
				db.Exec(`INSERT INTO security_scans(id,scanned_path,finding_count,status,created_at) VALUES(?,?,?,?,?)`,
					uuid.NewString()[:12], abs, len(findings), ternary(len(findings) > 0, "findings", "ok"), time.Now().UTC().Format(time.RFC3339))
			}
			if flagJSON {
				b, _ := json.MarshalIndent(findings, "", "  ")
				fmt.Println(string(b))
			} else if len(findings) == 0 {
				fmt.Printf("✓ No secrets found in %s\n", abs)
			} else {
				fmt.Printf("⚠ Found %d potential secret(s) in %s:\n", len(findings), abs)
				for _, f := range findings {
					fmt.Printf("  %s:%d  [%s]\n", f.File, f.LineNo, f.Pattern)
				}
				fmt.Println("\nNOTE: Matched values are NOT displayed for security.")
			}
			if len(findings) > 0 {
				return fmt.Errorf("%d findings", len(findings))
			}
			return nil
		}}
	scan.Flags().IntVar(&maxFiles, "max-files", 2000, "max files")
	list := &cobra.Command{Use: "list", Short: "List past scans",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT scanned_path,finding_count,status,created_at FROM security_scans ORDER BY created_at DESC LIMIT 20`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p, st, ts string
				var fc int
				if err := rows.Scan(&p, &fc, &st, &ts); err != nil {
					return err
				}
				fmt.Printf("%s  %d findings  [%s]  %s\n", p, fc, st, ts)
			}
			return nil
		}}
	s.AddCommand(scan, list)
	root.AddCommand(s)
}
