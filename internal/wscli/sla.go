package wscli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var slaWindowPattern = regexp.MustCompile(`^\d+[dhm]$`)

var slaCmd = &cobra.Command{
	Use:   "sla",
	Short: "Service-level agreement commands",
	Long:  `Read-only commands for viewing item SLA state and workspace compliance reports.`,
}

var slaItemCmd = &cobra.Command{
	Use:   "item <item-key-or-id>",
	Short: "Show the SLA state for an item",
	Long: `Show the read-only SLA state for a work item: ongoing and completed cycles,
elapsed and remaining time, breach and pause state, and calendar context.

Examples:
	ws sla item WI-123
	ws sla item 456`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		client, err := NewClient()
		if err != nil {
			return err
		}
		itemID, err := client.ResolveItemID(args[0])
		if err != nil {
			return fmt.Errorf("failed to resolve item: %w", err)
		}
		states, err := client.GetItemSLA(itemID)
		if err != nil {
			return fmt.Errorf("failed to get item SLA: %w", err)
		}
		output := NewOutput()
		output.Print(states)
		return nil
	},
}

var (
	slaReportFrom string
	slaReportTo   string

	slaAtRiskWithin  string
	slaAtRiskLimit   int
	slaBreachedLimit int
)

var slaReportCmd = &cobra.Command{
	Use:   "report [workspace-key]",
	Short: "Show the SLA compliance report for a workspace",
	Long: `Aggregate completed SLA cycles for a workspace. Uses the stored cycle
snapshots, so later configuration edits do not rewrite history.

Examples:
	ws sla report                # Uses the configured default workspace
	ws sla report PROJ --from 2025-01-01 --to 2025-04-01`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := NewClient()
		if err != nil {
			return err
		}
		wsKey := cfg.GetEffectiveWorkspace()
		if len(args) == 1 {
			wsKey = args[0]
		}
		if wsKey == "" {
			return fmt.Errorf("workspace is required: pass a workspace key or configure defaults.workspace_key")
		}
		workspaceID, err := client.ResolveWorkspaceID(wsKey)
		if err != nil {
			return fmt.Errorf("failed to resolve workspace: %w", err)
		}
		report, err := client.GetSLAReport(workspaceID, slaReportFrom, slaReportTo)
		if err != nil {
			return fmt.Errorf("failed to get SLA report: %w", err)
		}
		output := NewOutput()
		output.Print(report)
		return nil
	},
}

var slaAtRiskCmd = &cobra.Command{
	Use:   "at-risk",
	Short: "List items whose running SLA deadline is close or already past",
	Long: `List items in the selected workspace whose running SLA cycle has a deadline
within the given window. Items already marked breached have their deadline
cleared and are listed by "ws sla breached" instead.

Examples:
	ws sla at-risk -w PROJ                 # deadline within the next 2 days
	ws sla at-risk -w PROJ --within 12h
	ws sla at-risk -w PROJ --within 30m --limit 100`,
	RunE: func(_ *cobra.Command, _ []string) error {
		client, err := NewClient()
		if err != nil {
			return err
		}
		workspaceID, err := resolveRequiredWorkspace(client)
		if err != nil {
			return err
		}
		within, err := parseSLAWindow(slaAtRiskWithin)
		if err != nil {
			return err
		}
		query := fmt.Sprintf("slaRunning = true AND slaDeadline <= %s", within)
		items, err := client.ListSLAItems(query, &workspaceID, "sla_deadline", slaAtRiskLimit)
		if err != nil {
			return fmt.Errorf("failed to list at-risk items: %w", err)
		}
		output := NewOutput()
		output.Print(items)
		return nil
	},
}

var slaBreachedCmd = &cobra.Command{
	Use:   "breached",
	Short: "List items with a currently breached SLA cycle",
	Long: `List items in the selected workspace whose ongoing SLA cycle is breached.
Use "ws search --ql 'slaEverBreached = true'" to include historical breaches.

Examples:
	ws sla breached -w PROJ
	ws sla breached -w PROJ --limit 100`,
	RunE: func(_ *cobra.Command, _ []string) error {
		client, err := NewClient()
		if err != nil {
			return err
		}
		workspaceID, err := resolveRequiredWorkspace(client)
		if err != nil {
			return err
		}
		items, err := client.ListSLAItems("slaBreached = true", &workspaceID, "-updated_at", slaBreachedLimit)
		if err != nil {
			return fmt.Errorf("failed to list breached items: %w", err)
		}
		output := NewOutput()
		output.Print(items)
		return nil
	},
}

// parseSLAWindow validates a positive QL relative literal such as 2d, 12h, or
// 30m. Keeping the value unquoted matters: QL treats a quoted string as text,
// not as a relative instant.
func parseSLAWindow(value string) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if !slaWindowPattern.MatchString(trimmed) {
		return "", fmt.Errorf("invalid --within %q: use a whole number of days (d), hours (h), or minutes (m), e.g. 2d, 12h, 30m", value)
	}
	return trimmed, nil
}

func init() {
	rootCmd.AddCommand(slaCmd)
	slaCmd.AddCommand(slaItemCmd)
	slaCmd.AddCommand(slaReportCmd)
	slaCmd.AddCommand(slaAtRiskCmd)
	slaCmd.AddCommand(slaBreachedCmd)
	slaReportCmd.Flags().StringVar(&slaReportFrom, "from", "", "inclusive lower bound on the cycle stop time (RFC3339 or YYYY-MM-DD)")
	slaReportCmd.Flags().StringVar(&slaReportTo, "to", "", "exclusive upper bound on the cycle stop time (RFC3339 or YYYY-MM-DD)")
	slaAtRiskCmd.Flags().StringVar(&slaAtRiskWithin, "within", "2d", "deadline window: whole number of days (d), hours (h), or minutes (m), e.g. 2d, 12h, 30m")
	slaAtRiskCmd.Flags().IntVar(&slaAtRiskLimit, "limit", 50, "maximum items to return")
	slaBreachedCmd.Flags().IntVar(&slaBreachedLimit, "limit", 50, "maximum items to return")
}
