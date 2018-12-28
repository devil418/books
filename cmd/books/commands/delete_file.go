// Copyright © 2018 Tyler Spivey <tspivey@pcdesk.net> and Niko Carpenter <nikoacarpenter@gmail.com>
//
// This source code is governed by the MIT license, which can be found in the LICENSE file.

package commands

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/tspivey/books"
)

// If yes is true, no warning is going to be displayed if the file to be
// deleted is the last file of a book.
var yes bool

// The filename to be used instead of an ID.
var filename string

// delFileCmd represents the delete-file command.
var delFileCmd = &cobra.Command{
	Use:   "delete-file <file_id> | delete-file -f <filename>",
	Short: "Delete a single file from a book",
	Long: `Delete a single file from the library.

Deletes a single file with the provided ID or filename.
Use "books delete-file <id>" to delete by ID, or
"books delete-file -f <filename>" to delete by filename. If the provided
file is the only file for a book, the deletion will be aborted unless the -y
flag is used.`,
	Run: delFileFunc,
}

func init() {
	rootCmd.AddCommand(delFileCmd)

	delFileCmd.Flags().StringVarP(&filename, "filename", "f", "", "The filename to use instead of a file ID")
	delFileCmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete the file even if it's the last file of a book")
}

func delFileFunc(cmd *cobra.Command, args []string) {
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
		ID, err = lib.GetIDByFilename(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "can't get the ID of the passed file: %s\n", err)
		}
	}

	files, err := lib.GetFilesByID([]int64{ID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot get book file with ID %d: %s\n", err)
		os.Exit(1)
	}

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "File not found.")
		os.Exit(1)
	}

	bf := files[0]

	last, err := lib.IsLastFile(bf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error when checking whether the book has any more files: %s\n", err)
		os.Exit(1)
	}

	if !yes && last {
		fmt.Fprintln(os.Stderr, `This is the last file of a book.

Deleting this file will also delete the book associated with it.

If you're sure that you want to go ahead, pass the -y flag.`)
		os.Exit(2)
	}

	log.Printf("Deleting file %s (%d)", bf.CurrentFilename, bf.ID)
	if err := lib.DeleteFile(bf); err != nil {
		fmt.Fprintf(os.Stderr, "Error deleting file: %s\n", err)
	}

}

// getIDOrFilename returns an ID if a single, numeric argument was passed in
// args and the global variable "filename" is an empty string, a filename if
// args is empty and filename is not an empty string, an error otherwise.
//
// In any case, exactly one of the return values will be nonzero.
func getIDOrFilename(args []string) (ID int64, name string, err error) {
	if len(args) > 1 {
		return 0, "", errors.New("too many arguments")
	}
	if len(args) == 1 {
		if filename != "" {
			return 0, "", errors.New("Both a filename and an ID were provided")
		}
		ID, err = strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return 0, "", errors.Wrap(err, "The passed ID is not an integer")
		}
		return ID, "", nil
	}

	if filename == "" {
		return 0, "", errors.New("Neither a filename or an ID was provided.")
	}
	return 0, filename, nil
}
