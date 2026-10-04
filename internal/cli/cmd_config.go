// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"net/http"

	"github.com/spf13/cobra"
)

func (a *App) configCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Configuration schemas"}
	cmd.AddCommand(&cobra.Command{
		Use:   "schema <storage|node|daemon>",
		Short: "Print the JSON schema of a configuration",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var schema json.RawMessage
			if err := a.do(cmd.Context(), http.MethodGet, "/v1/config/schema/"+args[0], nil, &schema); err != nil {
				return err
			}
			var v any
			if err := json.Unmarshal(schema, &v); err != nil {
				return err
			}
			return a.json(v)
		},
	})
	return cmd
}
