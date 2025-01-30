/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// serverCmd represents the server command
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the server",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("server called")
	},
}

func init() {
	rootCmd.AddCommand(serverCmd)
}
