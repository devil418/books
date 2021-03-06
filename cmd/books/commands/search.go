// Copyright © 2018 Tyler Spivey <tspivey@pcdesk.net> and Niko Carpenter <nikoacarpenter@gmail.com>
//
// This source code is governed by the MIT license, which can be found in the LICENSE file.

package commands

import (
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/tspivey/books"

	"github.com/spf13/cobra"
)

// searchCmd represents the search command
var searchCmd = &cobra.Command{
	Use:   "search TERMS",
	Short: "Search the library",
	Long: `Search the library.
By default, all fields are searched. This can be overridden with field:value.
Supported fields: author, series, title, tags, extension.

Examples:
    Wizard's First Rule
    series:Sword+of+Truth
    author:Terry+Goodkind title:Phantom`,
	Run: CPUProfile(searchRun),
}

func searchRun(cmd *cobra.Command, args []string) {
	terms := strings.Join(args, " ")
	lib, err := books.OpenLibrary(libraryFile, booksRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open library: %s\n", err)
		os.Exit(1)
	}

	books, err := lib.Search(terms)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error while searching for books: %s\n", err)
		os.Exit(1)
	}
	resultTmplSrc := `{{range $i, $v := . -}}
{{joinNaturally "and" $v.Authors}} - {{$v.Title -}}
{{if $v.Series}} [{{$v.Series}}]{{end }} ({{ $v.ID }})
{{end}}`

	tmpl, err := template.New("search_result").Funcs(funcMap).Parse(resultTmplSrc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing template: %s\n", err)
		os.Exit(1)
	}

	err = tmpl.Execute(os.Stdout, books)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing template: %s\n", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(searchCmd)
}
