package cli

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tag-agent/tag/internal/store"
)

// registerEvalDataset wires versioned eval dataset management:
// eval-dataset create/add-case/list/export/delete.
// Port of src/tag/cmd/prd_clusters.py:cmd_eval_dataset + eval_datasets.py.
// `add-case` is a usability extension (Python only adds cases via eval import).
func registerEvalDataset(root *cobra.Command, app *App) {
	e := &cobra.Command{Use: "eval-dataset", Short: "Versioned eval dataset management", GroupID: "obs"}

	var description string
	create := &cobra.Command{Use: "create <name>", Short: "Create a new dataset", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			if _, ok, _ := getDataset(db, args[0]); ok {
				return fmt.Errorf("dataset already exists: %q", args[0])
			}
			id := uuid.NewString()[:12]
			_, err = db.Exec(`INSERT INTO eval_datasets(id,name,description,created_at,version,source_type,case_count,tags_json)
				VALUES(?,?,?,?,1,'manual',0,'[]')`, id, args[0], description, time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			fmt.Printf("Created dataset '%s' (id=%s, v1)\n", args[0], id)
			return nil
		}}
	create.Flags().StringVar(&description, "description", "", "dataset description")

	var expected, refContext string
	addCase := &cobra.Command{Use: "add-case <dataset> <case-id> <input>", Short: "Add a case to a dataset", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			dsID, ok, err := getDataset(db, args[0])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("dataset not found: %q", args[0])
			}
			var exp, ref any
			if cmd.Flags().Changed("expected") {
				exp = expected
			}
			if cmd.Flags().Changed("reference-context") {
				ref = refContext
			}
			if _, err := db.Exec(`INSERT INTO eval_dataset_cases(id,dataset_id,case_id,input,expected_output,reference_context,metadata_json,created_at)
				VALUES(?,?,?,?,?,?,'{}',?)`, uuid.NewString()[:12], dsID, args[1], args[2], exp, ref, time.Now().UTC().Format(time.RFC3339)); err != nil {
				return err
			}
			if _, err := db.Exec(`UPDATE eval_datasets SET case_count=case_count+1 WHERE id=?`, dsID); err != nil {
				return err
			}
			fmt.Printf("Added case '%s' to '%s'\n", args[1], args[0])
			return nil
		}}
	addCase.Flags().StringVar(&expected, "expected", "", "expected output (explicitly set, '' allowed)")
	addCase.Flags().StringVar(&refContext, "reference-context", "", "reference context")

	list := &cobra.Command{Use: "list", Short: "List datasets", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			rows, err := db.Query(`SELECT name, version, case_count FROM eval_datasets ORDER BY created_at DESC`)
			if err != nil {
				return err
			}
			defer rows.Close()
			type dsSummary struct {
				Name      string `json:"name"`
				Version   int    `json:"version"`
				CaseCount int    `json:"case_count"`
			}
			var out []dsSummary
			for rows.Next() {
				var d dsSummary
				rows.Scan(&d.Name, &d.Version, &d.CaseCount)
				out = append(out, d)
			}
			if flagJSON {
				return emitJSON(out)
			}
			for _, d := range out {
				fmt.Printf("%-40s v%d (%d cases)\n", d.Name, d.Version, d.CaseCount)
			}
			return nil
		}}

	var out string
	export := &cobra.Command{Use: "export <name>", Short: "Export a dataset to YAML", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			dsID, ok, err := getDataset(db, args[0])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("dataset not found: %q", args[0])
			}
			yamlStr, err := exportDatasetYAML(db, dsID)
			if err != nil {
				return err
			}
			if out != "" {
				if err := os.WriteFile(out, []byte(yamlStr), 0o644); err != nil {
					return err
				}
				fmt.Printf("Exported to %s\n", out)
				return nil
			}
			fmt.Print(yamlStr)
			return nil
		}}
	export.Flags().StringVar(&out, "out", "", "write YAML to file")

	del := &cobra.Command{Use: "delete <name>", Short: "Delete a dataset", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			dsID, ok, err := getDataset(db, args[0])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("dataset not found: %q", args[0])
			}
			if _, err := db.Exec(`DELETE FROM eval_dataset_cases WHERE dataset_id=?`, dsID); err != nil {
				return err
			}
			if _, err := db.Exec(`DELETE FROM eval_datasets WHERE id=?`, dsID); err != nil {
				return err
			}
			fmt.Printf("Deleted dataset '%s'\n", args[0])
			return nil
		}}

	e.AddCommand(create, addCase, list, export, del)
	root.AddCommand(e)
}

// getDataset resolves a dataset by id or name, returning (id, found, err).
func getDataset(db *store.DB, nameOrID string) (string, bool, error) {
	var id string
	err := db.QueryRow(`SELECT id FROM eval_datasets WHERE id=? OR name=?`, nameOrID, nameOrID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// yamlCase preserves field order and distinguishes an explicit "" expected_output
// (a *string) from an unset one (nil → omitted) — the C022 fix.
type yamlCase struct {
	ID       string  `yaml:"id"`
	Input    string  `yaml:"input"`
	Expected *string `yaml:"expected_output,omitempty"`
}

type yamlDoc struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Cases       []yamlCase `yaml:"cases"`
}

func exportDatasetYAML(db *store.DB, datasetID string) (string, error) {
	var name, description string
	if err := db.QueryRow(`SELECT name, description FROM eval_datasets WHERE id=?`, datasetID).Scan(&name, &description); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	rows, err := db.Query(`SELECT case_id, input, expected_output FROM eval_dataset_cases WHERE dataset_id=? ORDER BY created_at`, datasetID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	doc := yamlDoc{Name: name, Description: description, Cases: []yamlCase{}}
	for rows.Next() {
		var caseID, input string
		var expected sql.NullString
		if err := rows.Scan(&caseID, &input, &expected); err != nil {
			return "", err
		}
		c := yamlCase{ID: caseID, Input: input}
		if expected.Valid {
			v := expected.String
			c.Expected = &v
		}
		doc.Cases = append(doc.Cases, c)
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
