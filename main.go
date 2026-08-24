package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kuri4/dockerherocker/docker"
	"github.com/kuri4/dockerherocker/tui"
)

func main() {
	dcli, err := docker.NewClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to Docker: %v\n", err)
		os.Exit(1)
	}
	defer dcli.Close()

	m := tui.New(dcli)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}