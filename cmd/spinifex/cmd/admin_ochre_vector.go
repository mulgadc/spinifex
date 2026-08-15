package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

// defaultVectorEmbeddingModel is the platform's default embedding model for
// a new index when --model is not given.
const defaultVectorEmbeddingModel = "nomic-embed-text-v1.5"

// cliChunkPreviewChars bounds a chunk's preview in the query results table
// (D10 payload-size discipline extended to terminal readability): the full,
// server-truncated chunk text is always available via --json.
const cliChunkPreviewChars = 120

var ochreVectorCmd = &cobra.Command{
	Use:   "vector",
	Short: "Manage Ochre tenant vector-store indexes, ingestion and queries",
	Long: `Operator surface over the daemon's tenant vector store: create/delete/list
indexes, start an ingestion job from a predastore bucket, check a job's
status, and run a similarity query -- the live-verify surface for the
internal ochre.vector.* NATS API.`,
}

var ochreVectorIndexCmd = &cobra.Command{
	Use:   "index",
	Short: "Manage vector index lifecycle",
}

var ochreVectorIndexCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new vector index",
	Long: `create reserves a new index under the platform account, provisions its
account schema and backing table, and returns once it is READY. The index's
embedding model and dimension are fixed for its lifetime (D6): every ingest
and query against it uses exactly these values, regardless of what a caller
sends.`,
	Run: runOchreVectorIndexCreate,
}

var ochreVectorIndexDeleteCmd = &cobra.Command{
	Use:   "delete <index-id>",
	Short: "Delete a vector index",
	Long: `delete drops the index's backing table and its registry record. Idempotent:
deleting an already-absent index reports success.`,
	Args: cobra.ExactArgs(1),
	Run:  runOchreVectorIndexDelete,
}

var ochreVectorIndexListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every vector index",
	Run:   runOchreVectorIndexList,
}

var ochreVectorIngestCmd = &cobra.Command{
	Use:   "ingest",
	Short: "Start an ingestion job for a vector index from a predastore bucket",
	Long: `ingest lists --bucket/--prefix, chunks and embeds each object, and upserts
its rows into --index. The embedding model and dimension used are always the
index's own pinned values, not any --model flag -- an index's embedding
model is a property of the index, not of one ingest call. Returns as soon as
the job is claimed (PENDING); use 'job describe' to follow its progress.`,
	Run: runOchreVectorIngest,
}

var ochreVectorJobCmd = &cobra.Command{
	Use:   "job",
	Short: "Inspect vector-store ingestion jobs",
}

var ochreVectorJobDescribeCmd = &cobra.Command{
	Use:   "describe <job-id>",
	Short: "Show an ingestion job's current record",
	Args:  cobra.ExactArgs(1),
	Run:   runOchreVectorJobDescribe,
}

var ochreVectorQueryCmd = &cobra.Command{
	Use:   "query",
	Short: "Run a similarity query against a vector index",
	Long: `query embeds --text server-side against --index's own pinned embedding model
(D8) and returns the nearest chunks, nearest first. --filter takes a
JSON-encoded metadata Filter AST (Op/Key/Value/Children). Long chunk text is
truncated in the default table; pass --json for the full response.`,
	Run: runOchreVectorQuery,
}

func init() {
	ochreCmd.AddCommand(ochreVectorCmd)
	ochreVectorCmd.AddCommand(ochreVectorIndexCmd)
	ochreVectorIndexCmd.AddCommand(ochreVectorIndexCreateCmd)
	ochreVectorIndexCmd.AddCommand(ochreVectorIndexDeleteCmd)
	ochreVectorIndexCmd.AddCommand(ochreVectorIndexListCmd)
	ochreVectorCmd.AddCommand(ochreVectorIngestCmd)
	ochreVectorCmd.AddCommand(ochreVectorJobCmd)
	ochreVectorJobCmd.AddCommand(ochreVectorJobDescribeCmd)
	ochreVectorCmd.AddCommand(ochreVectorQueryCmd)

	ochreVectorIndexCreateCmd.Flags().String("name", "", "Human-readable index name (required)")
	ochreVectorIndexCreateCmd.Flags().Int("dimension", 0, "Embedding vector dimension (required)")
	ochreVectorIndexCreateCmd.Flags().String("model", defaultVectorEmbeddingModel, "Embedding model this index is pinned to")
	_ = ochreVectorIndexCreateCmd.MarkFlagRequired("name")
	_ = ochreVectorIndexCreateCmd.MarkFlagRequired("dimension")

	ochreVectorIngestCmd.Flags().String("index", "", "Target index ID (required)")
	ochreVectorIngestCmd.Flags().String("bucket", "", "Source predastore bucket (required)")
	ochreVectorIngestCmd.Flags().String("prefix", "", "Source object key prefix")
	ochreVectorIngestCmd.Flags().Int("chunk-size", 0, "Chunk size in runes (0 takes the package default)")
	ochreVectorIngestCmd.Flags().Int("chunk-overlap", 0, "Chunk overlap in runes (0 takes the package default)")
	ochreVectorIngestCmd.Flags().StringToString("meta", nil, "Static metadata tag stamped on every ingested row, k=v (repeatable)")
	_ = ochreVectorIngestCmd.MarkFlagRequired("index")
	_ = ochreVectorIngestCmd.MarkFlagRequired("bucket")

	ochreVectorQueryCmd.Flags().String("index", "", "Index ID to query (required)")
	ochreVectorQueryCmd.Flags().String("text", "", "Query text, embedded server-side against the index's pinned model (required)")
	ochreVectorQueryCmd.Flags().Int("k", 0, "Number of results (0 takes the service default; hard-capped at 100)")
	ochreVectorQueryCmd.Flags().String("filter", "", "JSON-encoded metadata Filter AST, e.g. {\"Op\":\"equals\",\"Key\":\"category\",\"Value\":\"faq\"}")
	ochreVectorQueryCmd.Flags().Bool("json", false, "Print full, untruncated results as JSON")
	_ = ochreVectorQueryCmd.MarkFlagRequired("index")
	_ = ochreVectorQueryCmd.MarkFlagRequired("text")
}

// vectorServiceFn indirects the NATS-backed client so the Run functions'
// connect/exit control flow can be tested without a live daemon, mirroring
// endpointServiceFn.
var vectorServiceFn = func() (handlers_ochrevector.VectorService, func(), error) {
	_, nc, err := loadConfigAndConnectFn()
	if err != nil {
		return nil, nil, err
	}
	return handlers_ochrevector.NewNATSVectorService(nc), nc.Close, nil
}

// formatIndexRecord renders one index record as aligned key/value lines.
func formatIndexRecord(rec handlers_ochrevector.Record) string {
	rows := [][2]string{
		{"Index ID", rec.ID},
		{"Name", rec.Name},
		{"State", rec.State},
		{"Dimension", strconv.Itoa(rec.Dimension)},
		{"Embedding model", rec.EmbeddingModel},
	}
	if !rec.CreatedAt.IsZero() {
		rows = append(rows, [2]string{"Created at", rec.CreatedAt.Format(time.RFC3339)})
	}
	out := ""
	for _, row := range rows {
		out += fmt.Sprintf("%-16s %s\n", row[0]+":", row[1])
	}
	return out
}

// formatJobRecord renders one ingestion job record as aligned key/value lines.
func formatJobRecord(job handlers_ochrevector.JobRecord) string {
	rows := [][2]string{
		{"Job ID", job.ID},
		{"Index ID", job.IndexID},
		{"State", job.State},
		{"Documents", fmt.Sprintf("%d/%d", job.DocumentsDone, job.DocumentsTotal)},
	}
	if len(job.FailedDocuments) > 0 {
		rows = append(rows, [2]string{"Failed docs", strconv.Itoa(len(job.FailedDocuments))})
	}
	if job.Error != "" {
		rows = append(rows, [2]string{"Error", job.Error})
	}
	if !job.CreatedAt.IsZero() {
		rows = append(rows, [2]string{"Created at", job.CreatedAt.Format(time.RFC3339)})
	}
	if !job.UpdatedAt.IsZero() {
		rows = append(rows, [2]string{"Updated at", job.UpdatedAt.Format(time.RFC3339)})
	}
	out := ""
	for _, row := range rows {
		out += fmt.Sprintf("%-14s %s\n", row[0]+":", row[1])
	}
	return out
}

// truncateForTable bounds s to maxRunes runes for a table cell, so one
// oversized chunk cannot blow out terminal width; the untruncated text is
// always available via --json.
func truncateForTable(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// formatQueryResults renders 'ochre vector query' output: a truncated table
// by default, or the full response as indented JSON when asJSON is set
// (D10's "full text via source ref" -- here, via --json).
func formatQueryResults(results []handlers_ochrevector.QueryResult, asJSON bool) (string, error) {
	if asJSON {
		//nolint:musttag // QueryResult is an internal, non-wire Go type (predates this CLI); it round-trips fine on its default field names.
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return "", fmt.Errorf("encode results as JSON: %w", err)
		}
		return string(data), nil
	}
	if len(results) == 0 {
		return "No results.", nil
	}
	tableData := pterm.TableData{{"SCORE", "SOURCE", "OFFSET", "CHUNK"}}
	for _, r := range results {
		tableData = append(tableData, []string{
			strconv.FormatFloat(float64(r.Score), 'f', 4, 32),
			r.SourceKey,
			strconv.Itoa(r.SourceOffset),
			truncateForTable(r.Chunk, cliChunkPreviewChars),
		})
	}
	return pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(tableData).Srender()
}

// parseFilterFlag decodes --filter's JSON-encoded Filter AST. An empty raw
// string is not an error: it means no filter, matching QueryRequest.Filter's
// nil-is-unfiltered contract.
func parseFilterFlag(raw string) (*handlers_ochrevector.Filter, error) {
	if raw == "" {
		return nil, nil
	}
	var f handlers_ochrevector.Filter
	//nolint:musttag // Filter is the existing filter-AST type (D9); it round-trips fine on its default field names.
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil, fmt.Errorf("decode --filter: %w", err)
	}
	return &f, nil
}

// runIndexCreate is the testable core of 'ochre vector index create': it
// mints a fresh index ID (indexes are named, not caller-numbered) and
// creates it. Returns the message to print.
func runIndexCreate(ctx context.Context, svc handlers_ochrevector.VectorService, name string, dimension int, model string) (string, error) {
	out, err := svc.CreateIndex(ctx, &handlers_ochrevector.CreateIndexRequest{
		IndexID:        utils.GenerateResourceID("idx"),
		Name:           name,
		Dimension:      dimension,
		EmbeddingModel: model,
	}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("✅ Created index %s.\n\n%s", out.Index.ID, formatIndexRecord(out.Index)), nil
}

// runIndexDelete is the testable core of 'ochre vector index delete'.
func runIndexDelete(ctx context.Context, svc handlers_ochrevector.VectorService, indexID string) (string, error) {
	if _, err := svc.DeleteIndex(ctx, &handlers_ochrevector.DeleteIndexRequest{IndexID: indexID}, utils.GlobalAccountID); err != nil {
		return "", err
	}
	return fmt.Sprintf("✅ Index %s deleted.", indexID), nil
}

// listIndexesOutput renders 'ochre vector index list'. Split from its Run
// function so it is testable against a fake service with no NATS connection.
func listIndexesOutput(ctx context.Context, svc handlers_ochrevector.VectorService) (string, error) {
	out, err := svc.ListIndexes(ctx, &handlers_ochrevector.ListIndexesRequest{}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}
	if len(out.Indexes) == 0 {
		return "No vector indexes.", nil
	}
	tableData := pterm.TableData{{"INDEX ID", "NAME", "STATE", "DIMENSION", "MODEL"}}
	for _, r := range out.Indexes {
		tableData = append(tableData, []string{r.ID, r.Name, r.State, strconv.Itoa(r.Dimension), r.EmbeddingModel})
	}
	return pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(tableData).Srender()
}

// runIngest is the testable core of 'ochre vector ingest'. The daemon stamps
// the index's own embedding model/dimension onto the source spec server-side
// (see handlers_ochrevector.vectorService.Ingest), so none is sent here.
func runIngest(ctx context.Context, svc handlers_ochrevector.VectorService, indexID, bucket, prefix string, chunkSize, chunkOverlap int, meta map[string]string) (string, error) {
	out, err := svc.Ingest(ctx, &handlers_ochrevector.IngestRequest{
		IndexID: indexID,
		Source: handlers_ochrevector.SourceSpec{
			Bucket:       bucket,
			Prefix:       prefix,
			ChunkSize:    chunkSize,
			ChunkOverlap: chunkOverlap,
			Metadata:     meta,
		},
	}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("✅ Started ingestion job %s for index %s (state %s).", out.Job.ID, indexID, out.Job.State), nil
}

// runJobDescribe is the testable core of 'ochre vector job describe'.
func runJobDescribe(ctx context.Context, svc handlers_ochrevector.VectorService, jobID string) (string, error) {
	out, err := svc.DescribeJob(ctx, &handlers_ochrevector.DescribeJobRequest{JobID: jobID}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}
	return formatJobRecord(out.Job), nil
}

// runQuery is the testable core of 'ochre vector query'.
func runQuery(ctx context.Context, svc handlers_ochrevector.VectorService, indexID, text string, k int, filterJSON string, asJSON bool) (string, error) {
	filter, err := parseFilterFlag(filterJSON)
	if err != nil {
		return "", err
	}
	out, err := svc.Query(ctx, &handlers_ochrevector.QueryRequest{IndexID: indexID, Text: text, K: k, Filter: filter}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}
	return formatQueryResults(out.Results, asJSON)
}

func runOchreVectorIndexCreate(cmd *cobra.Command, _ []string) {
	name, _ := cmd.Flags().GetString("name")
	dimension, _ := cmd.Flags().GetInt("dimension")
	model, _ := cmd.Flags().GetString("model")

	svc, closeFn, err := vectorServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	msg, err := runIndexCreate(context.Background(), svc, name, dimension, model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(msg)
}

func runOchreVectorIndexDelete(_ *cobra.Command, args []string) {
	svc, closeFn, err := vectorServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	msg, err := runIndexDelete(context.Background(), svc, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(msg)
}

func runOchreVectorIndexList(_ *cobra.Command, _ []string) {
	svc, closeFn, err := vectorServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	out, err := listIndexesOutput(context.Background(), svc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(out)
}

func runOchreVectorIngest(cmd *cobra.Command, _ []string) {
	indexID, _ := cmd.Flags().GetString("index")
	bucket, _ := cmd.Flags().GetString("bucket")
	prefix, _ := cmd.Flags().GetString("prefix")
	chunkSize, _ := cmd.Flags().GetInt("chunk-size")
	chunkOverlap, _ := cmd.Flags().GetInt("chunk-overlap")
	meta, _ := cmd.Flags().GetStringToString("meta")

	svc, closeFn, err := vectorServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	msg, err := runIngest(context.Background(), svc, indexID, bucket, prefix, chunkSize, chunkOverlap, meta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(msg)
}

func runOchreVectorJobDescribe(_ *cobra.Command, args []string) {
	svc, closeFn, err := vectorServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	out, err := runJobDescribe(context.Background(), svc, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Print(out)
}

func runOchreVectorQuery(cmd *cobra.Command, _ []string) {
	indexID, _ := cmd.Flags().GetString("index")
	text, _ := cmd.Flags().GetString("text")
	k, _ := cmd.Flags().GetInt("k")
	filterJSON, _ := cmd.Flags().GetString("filter")
	asJSON, _ := cmd.Flags().GetBool("json")

	svc, closeFn, err := vectorServiceFn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		ochreExit(1)
		return
	}
	defer closeFn()

	out, err := runQuery(context.Background(), svc, indexID, text, k, filterJSON, asJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(out)
}
