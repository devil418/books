// Copyright © 2018 Tyler Spivey <tspivey@pcdesk.net> and Niko Carpenter <nikoacarpenter@gmail.com>
//
// This source code is governed by the MIT license, which can be found in the LICENSE file.

package commands

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"github.com/tspivey/books"
)

// flag vars are declared in delete_file.go. We can't declare them again here as both files are in a single package.

// deleteCmd represents the delete command.
var deleteCmd = &cobra.Command{
	Use:   "delete <book_id> | delete -f <filename>",
	Short: "Delete a book",
	Long: `Delete a book from the library. If an ID is passed as a single argument
a book with that ID will be deleted. If the -f flag is used with a filename,
a book containing that file will be deleted. In both cases, all files the book contains will be removed.`,
	Run: deleteFunc,
}

func init() {
	rootCmd.AddCommand(deleteCmd)

	deleteCmd.Flags().StringVarP(&filename, "filename", "f", "", "The filename to use instead of a book ID")
}

func deleteFunc(cmd *cobra.Command, args []string) {
	ID, name, err := getIDOrFilename(args)
	if err != nil {
		fmt.Fprintf(os.Stdout, "Error: %s\n", err)
		os.Exit(1)
	}

	lib, err := books.OpenLibrary(libraryFile, booksRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening Library: %s\n", err)
		os.Exit(1)
	}
	defer lib.Close()

	if name != "" {
		ID, err = lib.GetBookIDByFilename(filename)
		if err != nil {
		fmt.Fprintf(os.Stderr, "Error retrieving book ID: %s\n", err)
		os.Exit(1)
		}
	}
	
	books, err  := lib.GetBooksByID([]int64{ID})
		if err != nil {
		fmt.Fprintf(os.Stderr, "Error retrieving book: %s\n", err)
		os.Exit(1)
	}
	if len(books) != 1 {
	fmt.Fprintln(os.Stderr, "Wrong number of books returned")
	os.Exit(1)
	}
	
	b := books[0]
	log.Printf("Deleting book \"%s\" (ID %d, %d files)", b.Title, b.ID, len(b.Files))
	
	// To delete a book, just delete all it's files. When deleting the last file, the book itself will be deleted automatically.
	for _, f := range b.Files {
	if err := lib.DeleteFile(f); err != nil {
	fmt.Fprintf(os.Stderr, "Error deleting file: %s", err)
	os.Exit(1)
	}
	}
}
