package manifest

import (
	"errors"
	"fmt"
	"slices"
)

// Notebook is an application view over sealed pages, not a transport or a
// second persistence engine. Page and index events use the normal draft core.
type Notebook struct {
	Op       string `json:"op"`
	Kind     string `json:"kind"`
	D        string `json:"d,omitempty"`
	Address  string `json:"address,omitempty"`
	Notebook string `json:"notebook,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Query    string `json:"query,omitempty"`
	Syntax   string `json:"syntax,omitempty"`
	Title    string `json:"title,omitempty"`
	Page     string `json:"page,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Order    string `json:"order,omitempty"`
	Reverse  string `json:"reverse,omitempty"`
	Limit    string `json:"limit,omitempty"`
	Section  string `json:"section,omitempty"`
	// Text is the text of a write, an append or a fix.
	Text string `json:"text,omitempty"`
}

func (m *Manifest) checkNotebook(n *Notebook, check, checkD func(string) error) error {
	if err := m.checkKind(n.Kind); err != nil {
		return err
	}
	if m.Kinds[n.Kind].NotebookIndex == "" {
		return errors.New("notebook action requires a notebook_index kind")
	}
	ops := []string{"read", "list", "search", "select", "next", "prev", "index", "toc"}
	writes := []string{"history", "write", "append", "fix", "delete"}
	if m.Kinds[n.Kind].NotebookEntry != "" {
		ops = append(ops, writes...)
	}
	if !slices.Contains(ops, n.Op) {
		if slices.Contains(writes, n.Op) {
			return fmt.Errorf("notebook %s requires a notebook_entry kind", n.Op)
		}
		return fmt.Errorf("invalid notebook operation %q", n.Op)
	}
	if slices.Contains(writes, n.Op) && (n.D == "" || n.Address == "") {
		return fmt.Errorf("notebook %s requires d and address", n.Op)
	}
	if n.Op == "read" && (n.D == "" || n.Address == "") {
		return errors.New("notebook read requires d and address")
	}
	if (n.Op == "next" || n.Op == "prev") && n.Address == "" {
		return errors.New("notebook navigation requires address")
	}
	if (n.Op == "index" || n.Op == "toc") && n.Notebook == "" {
		return errors.New("notebook index or toc requires notebook")
	}
	if err := checkD(n.D); err != nil {
		return err
	}
	for _, text := range []string{n.Address, n.Notebook, n.Prefix, n.Query, n.Syntax, n.Title, n.Page, n.From, n.To, n.Order, n.Reverse, n.Limit, n.Section, n.Text} {
		if err := check(text); err != nil {
			return err
		}
	}
	return nil
}
