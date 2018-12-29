// Copyright © 2018 Tyler Spivey <tspivey@pcdesk.net> and Niko Carpenter <nikoacarpenter@gmail.com>
//
// This source code is governed by the MIT license, which can be found in the LICENSE file.

package books

import (
	"database/sql"
	"database/sql/driver"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
)

// BookExistsError is returned by UpdateBook when a book with the given title and authors already exists in the database, and is not the one we're trying to update.
type BookExistsError struct {
	err    string
	BookID int64
}

func (bee BookExistsError) Error() string {
	return bee.err
}

var initialSchema = `create table books (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
series text,
title text not null
);
create index idx_books_title on books(title);

create table files (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
book_id integer references books(id) on delete cascade not null,
extension text not null,
original_filename text not null,
filename text not null unique,
file_size integer not null,
file_mtime timestamp not null,
hash text not null unique,
template_override text,
source text
);
create index idx_files_book_id on files(book_id);

create table authors (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
name text not null unique
);

create table books_authors (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
book_id integer not null references books(id) on delete cascade,
author_id integer not null references authors(id) on delete cascade,
unique (book_id, author_id)
);

create table tags (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
name text not null unique
);

create table files_tags (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
file_id integer not null references files(id) on delete cascade,
tag_id integer not null references tags(id) on delete cascade,
unique (file_id, tag_id)
);

create virtual table books_fts using fts4 (author, series, title, extension, tags,  filename, source);
`

func init() {
	// Add a connect hook to set synchronous = off for all connections.
	// This improves performance, especially during import,
	// but since changes aren't immediately synced to disk, data could be lost during a power outage or sudden OS crash.
	sql.Register("sqlite3async",
		&sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				conn.Exec("pragma foreign_keys=on", []driver.Value{})
				conn.Exec("pragma synchronous=off", []driver.Value{})
				return nil
			},
		})
}

// Library represents a set of books in persistent storage.
type Library struct {
	*sql.DB
	filename  string
	booksRoot string
}

// OpenLibrary opens a library stored in a file.
func OpenLibrary(filename, booksRoot string) (*Library, error) {
	db, err := sql.Open("sqlite3async", filename)
	if err != nil {
		return nil, err
	}
	return &Library{db, filename, booksRoot}, nil
}

// CreateLibrary initializes a new library in the specified file.
// Once CreateLibrary is called, the file will be ready to open and accept new books.
// Warning: This function sets up a new library for the first time. To get a Library based on an existing library file,
// call OpenLibrary.
func CreateLibrary(filename string) error {
	log.Printf("Creating library in %s\n", filename)
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		return errors.Wrap(err, "Create library")
	}
	defer db.Close()

	_, err = db.Exec(initialSchema)
	if err != nil {
		return errors.Wrap(err, "Create library")
	}

	log.Printf("Library created in %s\n", filename)
	return nil
}

// ImportBook adds a book to a library.
// The file referred to by book.OriginalFilename will either be copied or moved to the location referred to by book.CurrentFilename, relative to the configured books root.
// The book will not be imported if another book already in the library has the same hash.
func (lib *Library) ImportBook(book Book, tmpl *template.Template, move bool) error {
	if len(book.Files) != 1 {
		return errors.New("Book to import must contain only one file")
	}
	bf := &book.Files[0]
	tx, err := lib.Begin()
	if err != nil {
		return err
	}

	rows, err := tx.Query("select id from files where hash=?", bf.Hash)
	if err != nil {
		tx.Rollback()
		return err
	}
	if rows.Next() {
		// This book's hash is already in the library.
		var id int64
		rows.Scan(&id)
		tx.Rollback()
		return errors.Errorf("A duplicate book already exists with id %d", id)
	}

	rows.Close()
	if rows.Err() != nil {
		tx.Rollback()
		return errors.Wrapf(err, "Searching for duplicate book by hash %s", bf.Hash)
	}

	existingBookID, found, err := getBookIDByTitleAndAuthors(tx, book.Title, book.Authors)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "find existing book")
	}
	if !found {
		res, err := tx.Exec("insert into books (series, title) values(?, ?)", book.Series, book.Title)
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "Insert new book")
		}
		book.ID, err = res.LastInsertId()
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "sett new book ID")
		}
		for _, author := range book.Authors {
			if err := insertAuthor(tx, author, &book); err != nil {
				tx.Rollback()
				return errors.Wrapf(err, "inserting author %s", author)
			}
		}

	} else {
		book.ID = existingBookID
	}

	res, err := tx.Exec(`insert into files (book_id, extension, original_filename, filename, file_size, file_mtime, hash, source)
	values (?, ?, ?, ?, ?, ?, ?, ?)`,
		book.ID, bf.Extension, bf.OriginalFilename, bf.CurrentFilename, bf.FileSize, bf.FileMtime, bf.Hash, bf.Source)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Inserting book file into the db")
	}

	id, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Fetching new book ID")
	}
	book.Files[0].ID = id

	for _, tag := range bf.Tags {
		if err := insertTag(tx, tag, bf); err != nil {
			tx.Rollback()
			return errors.Wrapf(err, "inserting tag %s", tag)
		}
	}

	err = indexBookInSearch(tx, &book, !found)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "index book in search")
	}

	err = lib.updateFilenames(tx, book, tmpl, move)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Moving or copying book")
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "import book")
	}
	log.Printf("Imported book: %s: %s, ID = %d", strings.Join(book.Authors, " & "), book.Title, book.ID)

	return nil
}

func indexBookInSearch(tx *sql.Tx, book *Book, createNew bool) error {
	bf := book.Files[0]
	joinedTags := strings.Join(bf.Tags, " ")
	if createNew {
		// Index book for searching.
		extensions := []string{}
		tags := []string{}
		sources := []string{}
		for _, f := range book.Files {
			tags = append(tags, f.Tags...)
			extensions = append(extensions, f.Extension)
			sources = append(sources, f.Source)
		}

		_, err := tx.Exec(`insert into books_fts (docid, author, series, title, extension, tags,  source)
	values (?, ?, ?, ?, ?, ?, ?)`,
			book.ID, strings.Join(book.Authors, " & "), book.Series, book.Title, strings.Join(extensions, " "), strings.Join(tags, " "), strings.Join(sources, " "))
		if err != nil {
			return err
		}
		return nil
	}
	rows, err := tx.Query("select docid, tags, extension, source from books_fts where docid=?", book.ID)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rows.Close()
		if rows.Err() != nil {
			return err
		}
		return errors.Errorf("Existing book %d not found in FTS", book.ID)
	}
	var id int64
	var tags, extension, source string
	err = rows.Scan(&id, &tags, &extension, &source)
	if err != nil {
		return err
	}
	rows.Close()

	_, err = tx.Exec("update books_fts set tags=?, extension=?, source=? where docid=?", tags+" "+joinedTags, extension+" "+bf.Extension, source+" "+bf.Source, id)
	if err != nil {
		return err
	}
	return nil
}

// insertAuthor inserts an author into the database.
func insertAuthor(tx *sql.Tx, author string, book *Book) error {
	var authorID int64
	row := tx.QueryRow("select id from authors where name=?", author)
	err := row.Scan(&authorID)
	if err == sql.ErrNoRows {
		// Insert the author
		res, err := tx.Exec("insert into authors (name) values(?)", author)
		if err != nil {
			return err
		}
		authorID, err = res.LastInsertId()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Author inserted, insert the link
	// For two authors in the same book with the same name, only insert one.
	if _, err := tx.Exec("insert or ignore into books_authors (book_id, author_id) values(?, ?)", book.ID, authorID); err != nil {
		return err
	}
	return nil
}

// insertTag inserts a tag into the database.
func insertTag(tx *sql.Tx, tag string, bf *BookFile) error {
	var tagID int64
	row := tx.QueryRow("select id from tags where name=?", tag)
	err := row.Scan(&tagID)
	if err == sql.ErrNoRows {
		// Insert the tag
		res, err := tx.Exec("insert into tags (name) values(?)", tag)
		if err != nil {
			return err
		}
		tagID, err = res.LastInsertId()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Tag inserted, insert the link
	// Avoid duplicate tags.
	if _, err := tx.Exec("insert or ignore into files_tags (file_id, tag_id) values(?, ?)", bf.ID, tagID); err != nil {
		return err
	}
	return nil
}

// Search searches the library for books.
// By default, all fields are searched, but
// field:terms+to+search will limit to that field only.
// Fields: author, title, series, extension, tags, filename, source.
// Example: author:Stephen+King title:Shining
func (lib *Library) Search(terms string) ([]Book, error) {
	books, _, err := lib.SearchPaged(terms, 0, 0, 0)
	return books, err
}

// SearchPaged implements book searching, both paged and non paged.
// Set limit to 0 to return all results.
// moreResults will be set to the number of additional results not returned, with a maximum of moreResultsLimit.
func (lib *Library) SearchPaged(terms string, offset, limit, moreResultsLimit int) (books []Book, moreResults int, err error) {
	books = []Book{}
	var query string
	args := []interface{}{terms}
	if limit == 0 {
		query = "select docid from books_fts where books_fts match ?"
	} else {
		query = "select docid from books_fts where books_fts match ? LIMIT ? OFFSET ?"
		args = append(args, limit+moreResultsLimit, offset)
	}

	rows, err := lib.Query(query, args...)
	if err != nil {
		return nil, 0, errors.Wrap(err, "Querying db for search terms")
	}

	var ids []int64
	var id int64
	for rows.Next() {
		rows.Scan(&id)
		ids = append(ids, id)
	}
	err = rows.Err()
	if err != nil {
		return nil, 0, errors.Wrap(err, "Retrieving search results from db")
	}

	if limit > 0 && len(ids) > limit {
		moreResults = len(ids) - limit
		ids = ids[:limit]
	}
	books, err = lib.GetBooksByID(ids)

	return
}

// GetBooksByID retrieves books from the library by their id.
func (lib *Library) GetBooksByID(ids []int64) ([]Book, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	tx, err := lib.Begin()
	if err != nil {
		return nil, errors.Wrap(err, "get books by ID")
	}
	books, err := getBooksByID(tx, ids)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	err = tx.Commit()
	if err != nil {
		return nil, errors.Wrap(err, "get books by ID")
	}
	return books, nil
}

// getBooksByID retrieves books from the library by their id.
func getBooksByID(tx *sql.Tx, ids []int64) ([]Book, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	results := []Book{}

	query := "select id, series, title from books where id in (" + joinInt64s(ids, ",") + ")"
	rows, err := tx.Query(query)
	if err != nil {
		return results, errors.Wrap(err, "fetching books from database by ID")
	}

	for rows.Next() {
		book := Book{}
		if err := rows.Scan(&book.ID, &book.Series, &book.Title); err != nil {
			return nil, errors.Wrap(err, "scanning rows")
		}

		results = append(results, book)
	}

	if rows.Err() != nil {
		return nil, errors.Wrap(err, "querying books by ID")
	}
	rows.Close()

	authorMap, err := getAuthorsByBookIds(tx, ids)
	if err != nil {
		return nil, errors.Wrap(err, "get authors for books")
	}

	fileMap, err := getFilesByBookIds(tx, ids)
	if err != nil {
		return nil, errors.Wrap(err, "get files for books")
	}

	// Get authors and files
	for i, book := range results {
		results[i].Authors = authorMap[book.ID]
		results[i].Files = fileMap[book.ID]
	}
	return results, nil
}

// getAuthorsByBookIds gets author names for each book ID.
func getAuthorsByBookIds(tx *sql.Tx, ids []int64) (map[int64][]string, error) {
	m := make(map[int64][]string)
	if len(ids) == 0 {
		return m, nil
	}

	var bookID int64
	var authorName string

	query := "SELECT ba.book_id, a.name FROM books_authors ba JOIN authors a ON ba.author_id = a.id WHERE ba.book_id IN (" + joinInt64s(ids, ",") + ") ORDER BY ba.id"
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		err := rows.Scan(&bookID, &authorName)
		if err != nil {
			return nil, err
		}
		authors := m[bookID]
		m[bookID] = append(authors, authorName)
	}

	return m, nil
}

// getTagsByFileIds gets tag names for each book ID.
func getTagsByFileIds(tx *sql.Tx, ids []int64) (map[int64][]string, error) {
	tagsMap := make(map[int64][]string)
	if len(ids) == 0 {
		return nil, nil
	}

	var fileID int64
	var tag string

	query := "SELECT ft.file_id, t.name FROM files_tags ft JOIN tags t ON ft.tag_id = t.id WHERE ft.file_id IN (" + joinInt64s(ids, ",") + ") ORDER BY ft.id"
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		err := rows.Scan(&fileID, &tag)
		if err != nil {
			return nil, err
		}
		tagsMap[fileID] = append(tagsMap[fileID], tag)
	}

	return tagsMap, nil
}

// getFilesByBookIds gets files for each book ID.
func getFilesByBookIds(tx *sql.Tx, ids []int64) (fileMap map[int64][]BookFile, err error) {
	if len(ids) == 0 {
		return nil, nil
	}
	fileIDMap := make(map[int64][]int64)
	fileMap = make(map[int64][]BookFile)

	query := "select id, book_id from files where book_id in (" + joinInt64s(ids, ",") + ")"
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bookID, fileID int64
	for rows.Next() {
		err := rows.Scan(&fileID, &bookID)
		if err != nil {
			return nil, err
		}
		fileIDMap[bookID] = append(fileIDMap[bookID], fileID)
	}

	for bookID, fileIDs := range fileIDMap {
		files, err := getFilesByID(tx, fileIDs)
		if err != nil {
			return nil, err
		}
		fileMap[bookID] = files
	}

	return fileMap, nil
}

// GetFilesByID gets files for each ID.
func (lib *Library) GetFilesByID(ids []int64) ([]BookFile, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	tx, err := lib.Begin()
	if err != nil {
		return nil, err
	}

	files, err := getFilesByID(tx, ids)
	if err != nil {
		tx.Rollback()
	} else {
		tx.Commit()
	}

	return files, err
}

// GetFilesById gets files for each ID.
func getFilesByID(tx *sql.Tx, ids []int64) ([]BookFile, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	files := []BookFile{}
	tagMap, err := getTagsByFileIds(tx, ids)
	if err != nil {
		return nil, err
	}
	query := "select id, extension, original_filename, filename, file_size, file_mtime, hash, source from files where id in (" + joinInt64s(ids, ",") + ")"
	rows, err := tx.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		bf := BookFile{}
		err := rows.Scan(&bf.ID, &bf.Extension, &bf.OriginalFilename, &bf.CurrentFilename, &bf.FileSize, &bf.FileMtime, &bf.Hash, &bf.Source)
		if err != nil {
			return nil, err
		}
		bf.Tags = tagMap[bf.ID]
		files = append(files, bf)
	}
	return files, nil
}

// ConvertToEpub converts a file to epub, and caches it in LIBRARY_ROOT/cache.
// This depends on ebook-convert, which takes the original filename, and the new filename, in that order.
// the file's hash, with the extension .epub, will be the name of the cached file.
func (lib *Library) ConvertToEpub(file BookFile) error {
	filename := path.Join(lib.booksRoot, file.CurrentFilename)
	cacheDir := path.Join(path.Dir(lib.filename), "cache")
	newFile := path.Join(cacheDir, file.Hash+".epub")
	cmd := exec.Command("ebook-convert", filename, newFile)
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

// UpdateBook updates the authors and title of an existing book in the database, specified by book.ID.
func (lib *Library) UpdateBook(book Book, tmpl *template.Template, updateSeries bool) error {
	tx, err := lib.Begin()
	if err != nil {
		return errors.Wrap(err, "get transaction")
	}
	existingBooks, err := getBooksByID(tx, []int64{book.ID})
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "get books by ID")
	}
	if len(existingBooks) == 0 {
		tx.Rollback()
		return errors.New("book not found")
	}
	existingBook := existingBooks[0]
	if existingBook.Title == book.Title &&
		authorsEqual(existingBook.Authors, book.Authors) &&
		(!updateSeries || existingBook.Series == book.Series) {
		tx.Rollback()
		log.Printf("Not updating book %d because nothing changed", book.ID)
		return nil
	}
	existingBookID, found, err := getBookIDByTitleAndAuthors(tx, book.Title, book.Authors)
	if err != nil {
		return errors.Wrap(err, "find existing book")
	}
	if found && book.ID != existingBookID {
		tx.Rollback()
		return BookExistsError{"Book already exists", existingBookID}
	}

	if book.Title != existingBook.Title ||
		(updateSeries && book.Series != existingBook.Series) {
		if updateSeries {
			_, err = tx.Exec("update books set updated_on=datetime(), title=?, series=? where id=?", book.Title, book.Series, book.ID)
		} else {
			_, err = tx.Exec("update books set updated_on=datetime(), title=? where id=?", book.Title, book.ID)
		}
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "update title")
		}
	}
	if !authorsEqual(existingBook.Authors, book.Authors) {
		_, err := tx.Exec("delete from books_authors where book_id=?", book.ID)
		if err != nil {
			tx.Rollback()
			return errors.Wrap(err, "delete authors")
		}
		for _, author := range book.Authors {
			if err := insertAuthor(tx, author, &book); err != nil {
				tx.Rollback()
				return errors.Wrap(err, "insert author")
			}
		}
	}
	_, err = tx.Exec("update books_fts set title=?, author=? where docid=?", book.Title, strings.Join(book.Authors, " & "), book.ID)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "update book")
	}
	err = lib.updateFilenames(tx, book, tmpl, true)
	if err != nil {
		log.Printf("Error updating filenames: %s", err)
	}
	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "update book")
	}
	log.Printf("Updated book %d with authors: %s title: %s", book.ID, strings.Join(book.Authors, " & "), book.Title)
	return nil
}

// GetBookIDByTitleAndAuthors gets an existing book ID with the given title and authors.
func (lib *Library) GetBookIDByTitleAndAuthors(title string, authors []string) (int64, bool, error) {
	tx, err := lib.Begin()
	if err != nil {
		return 0, false, errors.Wrap(err, "get transaction")
	}
	defer tx.Rollback()
	return getBookIDByTitleAndAuthors(tx, title, authors)
}

func getBookIDByTitleAndAuthors(tx *sql.Tx, title string, authors []string) (int64, bool, error) {
	rows, err := tx.Query("SELECT id FROM books WHERE title = ?", title)
	if err != nil {
		return 0, false, errors.Wrap(err, "get book by title")
	}

	var id int64
	ids := make([]int64, 0)
	for rows.Next() {
		err := rows.Scan(&id)
		if err != nil {
			return 0, false, errors.Wrap(err, "Get book ID from title")
		}

		ids = append(ids, id)
	}
	rows.Close()

	authorMap, err := getAuthorsByBookIds(tx, ids)
	if err != nil {
		return 0, false, errors.Wrap(err, "get authors for books")
	}

	for bookID, authorNames := range authorMap {
		if authorsEqual(authors, authorNames) {
			return bookID, true, nil
		}
	}

	return 0, false, nil
}

// MergeBooks merges all of the files from ids into the first one.
func (lib *Library) MergeBooks(ids []int64, tmpl *template.Template) error {
	tx, err := lib.Begin()
	if err != nil {
		return errors.Wrap(err, "create transaction")
	}
	if err := lib.mergeBooks(tx, ids, tmpl); err != nil {
		tx.Rollback()
		return errors.Wrap(err, "merge books")
	}
	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "commit")
	}
	return nil
}

func (lib *Library) mergeBooks(tx *sql.Tx, ids []int64, tmpl *template.Template) error {
	_, err := tx.Exec("update files set updated_on=datetime(), book_id=? where book_id in ("+joinInt64s(ids[1:], ",")+")", ids[0])
	if err != nil {
		return errors.Wrap(err, "merge books")
	}
	if _, err = tx.Exec("delete from books where id in (" + joinInt64s(ids[1:], ",") + ")"); err != nil {
		return errors.Wrap(err, "delete book")
	}
	if _, err = tx.Exec("delete from books_fts where docid in (" + joinInt64s(ids[1:], ",") + ")"); err != nil {
		return errors.Wrap(err, "delete from books_fts")
	}
	// Reindex the book in search
	_, err = tx.Exec("delete from books_fts where docid=?", ids[0])
	if err != nil {
		return errors.Wrap(err, "delete original book from fts")
	}
	books, err := getBooksByID(tx, []int64{ids[0]})
	if err != nil {
		return errors.Wrap(err, "get original book")
	}
	if len(books) == 0 {
		return errors.New("Can't find original book to reindex")
	}
	if err := indexBookInSearch(tx, &books[0], true); err != nil {
		return errors.Wrap(err, "index book in search")
	}
	if err := lib.updateFilenames(tx, books[0], tmpl, true); err != nil {
		return errors.Wrap(err, "update filenames")
	}
	return nil
}

// GetIDByFilename returns a file ID given a filename relative to books root.
func (lib *Library) GetIDByFilename(fn string) (ID int64, err error) {
	tx, err := lib.Begin()
	if err != nil {
		return 0, errors.Wrap(err, "cannot start transaction")
	}
	defer tx.Rollback()

	row := tx.QueryRow("select id from files where filename = ?", fn)
	if err := row.Scan(&ID); err != nil {
		if err == sql.ErrNoRows {
			return 0, errors.New("no file with that name exists")
		}
		return 0, err
	}
	return ID, nil
}

// GetBookIDByFilename returns a book ID given a filename relative to books root.
func (lib *Library) GetBookIDByFilename(fn string) (int64, error) {
	tx, err := lib.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query("Select book_id from files where filename=?", fn)
	if err != nil {
		return 0, err
	}
	if rows.Next() {
		var id int64
		rows.Scan(&id)
		return id, nil
	}
	rows.Close()
	return 0, errors.New("book not found")
}

func (lib *Library) updateFilenames(tx *sql.Tx, book Book, tmpl *template.Template, move bool) error {
	for _, bf := range book.Files {
		if bf.ID == 0 {
			return errors.New("ID cannot be 0")
		}
		newFn, err := bf.Filename(tmpl, &book)
		if err != nil {
			return errors.Wrap(err, "get new filename")
		}
		newFn = TruncateFilename(newFn)
		if bf.CurrentFilename == newFn {
			continue
		}
		newPath, err := GetUniqueName(filepath.Join(lib.booksRoot, newFn))
		if err != nil {
			return errors.Wrap(err, "get unique name")
		}
		relPath, err := filepath.Rel(lib.booksRoot, newPath)
		if err != nil {
			return errors.Wrap(err, "get relative path")
		}
		var cf string
		if filepath.IsAbs(bf.CurrentFilename) {
			// Importing this book
			cf = bf.CurrentFilename
		} else {
			cf = filepath.Join(lib.booksRoot, bf.CurrentFilename)
		}
		if err := moveOrCopyFile(cf, newPath, move); err != nil {
			return errors.Wrap(err, "move or copy file")
		}
		if _, err := tx.Exec("update files set updated_on=datetime(), filename=? where id=?", relPath, bf.ID); err != nil {
			return errors.Wrap(err, "updating file")
		}
	}
	return nil
}

func authorsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// isLastFile returns true if the book associated with the passed file has no
// other associated files.
func (lib *Library) IsLastFile(bf BookFile) (last bool, err error) {
	bookID, err := lib.GetBookIDByFilename(bf.CurrentFilename)
	if err != nil {
		return false, err
	}

	books, err := lib.GetBooksByID([]int64{bookID})
	if err != nil {
		return false, err
	}

	if len(books) != 1 {
		panic("Internal database inconsistency, this should NOT happen.")
	}
	return len(books[0].Files) == 1, nil
}

//  DeleteFile deletes the passed file.
// If the book associated with it has no more files, it's also deleted, along
// with any authors and tags that wouldn't have had any associated books after
// the deletion.
func (lib *Library) DeleteFile(bf BookFile) (err error) {
	tx, err := lib.Begin()
	if err != nil {
		return err
	}
	defer func() {
		// Returning something assigns that to our named return value implicitly.
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()

	if err := lib.deleteFile(tx, bf); err != nil {
		return err
	}
	return nil
}

func (lib *Library) deleteFile(tx *sql.Tx, bf BookFile) error {
	// Retrieve the information needed for cleanup that won't be available after the file is gone.	
	last, err := lib.IsLastFile(bf)
	if err != nil {
		return err
	}

	// get the book ID:
	var bID int64
	row := tx.QueryRow("select book_id from files where id = ?", bf.ID)
	if err := row.Scan(&bID); err != nil {
		return err
	}

	// Remove the tags not associated with any other file:
	if err := cleanupTags(tx, bf.ID); err != nil {
		return errors.Wrap(err, "error when cleaning up tags")
	}

	// Delete file from the database:
	if _, err := tx.Exec("delete from files where id = ?", bf.ID); err != nil {
		return err
	}

	// delete from disk:
	if err := os.Remove(path.Join(lib.booksRoot, bf.CurrentFilename)); err != nil {
		log.Printf("Cannot delete %s from the file system: %s\nYou should delete the file manually.", bf.CurrentFilename, err)
	}

	// Get the book the file was associated with:
		books, err := lib.GetBooksByID([]int64{bID})
	if err != nil {
		return err
	}
	if len(books) != 1 {
		return errors.New("wrong NO of books returned")
	}
	b := books[0]

	if last { //Delete the associated book
		if err := lib.deleteBook(tx, b); err != nil {
			return errors.Wrap(err, "cannot delete book")
		}
	}
	
	// Delete file from search:
	// This code is a bit of a hack, but there's no easy way to do it better.
	// Deleting it the proper way would involve figuring out which tags, extensions etc.  aren't relevant to that book any more and removing them.
	// This is not an easy thing to do, as it would involve scanning through all the files and breaking the strings from
	// the fts table into parts. It's not worth it and the performance win, if any, would be negligible.
	// Instead we delete the whole book from fts and just re-index it.
	if err := lib.deleteBookFromSearch(tx, b); err != nil {
		return err
	}
	return indexBookInSearch(tx, &b, true)
}

// cleanupTags removes any tags not associated with any other files.
func cleanupTags(tx *sql.Tx, id int64) error {
	// Get all the tags for the current file:
	rows, err := tx.Query("select tag_id from files_tags where file_id = ?", id)
	if err != nil {
		return err
	}
	defer rows.Close()

	var tags []int64
	for rows.Next() {
		var tag int64
		if err := rows.Scan(&tag); err != nil {
			return err
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// tags that are only associated with one file should be deleted.
	var toDel []int64
	for _, t := range tags {
		// Check how many files this tag is associated with.
		row := tx.QueryRow("select count(*) from files_tags where tag_id = ?", t)
		var count int64
		if err := row.Scan(&count); err != nil {
			return errors.Wrap(err, "error when getting the count of files associated with a tag")
		}
		if count == 1 {
			toDel = append(toDel, t)
		}
	}

	if toDel != nil {
		toDelS := joinInt64s(toDel, ",")
		log.Printf("Deleting tags %s", toDelS)
		if _, err := tx.Exec("delete from tags where id in (" + toDelS + ")"); err != nil {
			return errors.Wrap(err, "error when deleting tags")
		}
	}
	return nil
}

func (lib *Library) deleteBook(tx *sql.Tx, b Book) error {
	log.Printf("Deleting book %d", b.ID)
	if err := lib.cleanupAuthors(tx, b); err != nil {
		return errors.Wrap(err, "cannot clean up authors")
	}
	if _, err := tx.Exec("delete from books where id=?", b.ID); err != nil {
		return errors.Wrap(err, "can't delete book")
	}

	if err := lib.deleteBookFromSearch(tx, b); err != nil {
		return errors.Wrap(err, "cannot delete the book from the search index")
	}
	return nil
}

func (lib *Library) deleteBookFromSearch(tx *sql.Tx, b Book) error {
	_, err := tx.Exec("delete from books_fts where docid=?", b.ID)
	return err
}

func (lib *Library) cleanupAuthors(tx *sql.Tx, b Book) error {
	rows, err := tx.Query("select author_id from books_authors where book_id=?", b.ID)
	if err != nil {
		return errors.Wrap(err, "cannot retrieve authors for book")
	}
	defer rows.Close()
	authors := make([]int64, 0)
	for rows.Next() {
		var author int64
		if err := rows.Scan(&author); err != nil {
			return errors.Wrap(err, "cannot scan author")
		}
		authors = append(authors, author)
	}

	// Find authors that have no other books associated with them:
	toDel := make([]int64, 0)
	for _, author := range authors {
		row := tx.QueryRow("select count(*) from books_authors where author_id=?", author)
		var count int
		if err := row.Scan(&count); err != nil {
			return errors.Wrap(err, "error retrieving count of associated books")
		}
		if count == 1 {
			toDel = append(toDel, author)
		}
	}

	authorsStr := joinInt64s(toDel, ",")
	log.Printf("Deleting authors: %s ", authorsStr)
	_, err = tx.Exec("delete from authors where id in (" + authorsStr + ")")
	if err != nil {
		return errors.Wrap(err, "cannot delete authors")
	}
	return nil
}

// joinInt64s is like strings.Join, but for slices of int64.
// SQLite limits the number of variables that can be passed to a bound query.
// Pass int64s directly to IN (…) as a work-around.
// query := “SELECT * FROM table WHERE id IN (“ + joinInt64s(ids, “,”) + “)”
func joinInt64s(items []int64, sep string) string {
	itemsStr := make([]string, len(items))
	for i, item := range items {
		itemsStr[i] = strconv.FormatInt(item, 10)
	}

	return strings.Join(itemsStr, sep)
}
