/*
Copyright © 2025 Ahmad Anvari <EMAIL ADDRESS>
*/
package main

import (
	"os"

	"github.com/spf13/cobra"
)

var cfgFile string

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "futureq",
	Short: "FutureQ server",
	Long:  `FutureQ is a highly available distrubuted scheduled message queue`,
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "Path to config file")

	rootCmd.AddCommand(startCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
