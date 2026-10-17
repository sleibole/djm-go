package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

const (
	maxCatalogsPerAccount = 100
	maxCatalogNameLen     = 100
	maxUploadBytes        = 100 << 20
	uploadTimeout         = 10 * time.Minute
	catalogSearchLimit    = 50
)

var (
	errNoFile       = errors.New("No file provided")
	errFileTooLarge = errors.New("File too large. Maximum size is 100MB.")
)

// requireHost is requireUser plus the DJ/KJ role.
func (a *app) requireHost(next http.HandlerFunc) http.HandlerFunc {
	return a.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).Hosts() {
			a.message(w, r, http.StatusForbidden, "This page is for hosts",
				"Start hosting from your account page to run shows and upload song catalogs.")
			return
		}
		next(w, r)
	})
}

// ownCatalog loads the {id} catalog if it belongs to the current user; anyone
// else gets a 404.
func (a *app) ownCatalog(w http.ResponseWriter, r *http.Request) (Catalog, bool) {
	var c Catalog
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return c, false
	}
	err = a.db.Where("id = ? AND user_id = ?", id, currentUser(r).ID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		a.notFound(w, r)
		return c, false
	}
	if err != nil {
		a.serverError(w, r, err)
		return c, false
	}
	return c, true
}

type catalogsPage struct {
	Catalogs []Catalog
	Saved    string
	AtLimit  bool
}

func (a *app) handleCatalogs(w http.ResponseWriter, r *http.Request) {
	var cs []Catalog
	if err := a.db.Where("user_id = ?", currentUser(r).ID).Order("name").Find(&cs).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "catalogs.html", catalogsPage{
		Catalogs: cs, Saved: r.URL.Query().Get("saved"), AtLimit: len(cs) >= maxCatalogsPerAccount,
	})
}

type newCatalogPage struct {
	Name  string
	Error string
}

func (a *app) handleNewCatalog(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "catalog_new.html", newCatalogPage{})
}

func (a *app) handleCreateCatalog(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	name := strings.TrimSpace(r.PostFormValue("name"))
	fail := func(status int, msg string) {
		a.render(w, r, status, "catalog_new.html", newCatalogPage{Name: name, Error: msg})
	}
	if name == "" {
		fail(http.StatusUnprocessableEntity, "Name can't be blank")
		return
	}
	if utf8.RuneCountInString(name) > maxCatalogNameLen {
		fail(http.StatusUnprocessableEntity, fmt.Sprintf("Name is too long (maximum is %d characters)", maxCatalogNameLen))
		return
	}
	var count, taken int64
	if err := a.db.Model(&Catalog{}).Where("user_id = ?", u.ID).Count(&count).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	if count >= maxCatalogsPerAccount {
		fail(http.StatusUnprocessableEntity, fmt.Sprintf("You can have up to %d catalogs. Delete one to make room.", maxCatalogsPerAccount))
		return
	}
	if err := a.db.Model(&Catalog{}).Where("user_id = ? AND name = ?", u.ID, name).Count(&taken).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	if taken > 0 {
		fail(http.StatusUnprocessableEntity, "Name has already been taken")
		return
	}
	c := Catalog{UserID: u.ID, Name: name}
	if err := a.db.Create(&c).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/catalogs/%d?saved=created", c.ID), http.StatusSeeOther)
}

type catalogPage struct {
	Catalog        Catalog
	Metadata       importMetadata
	Saved          string
	Built          bool // the page was loaded because a build just finished
	UploadError    string
	DeleteError    string
	VariantChoices []choice
	Search         catalogSearchView
}

type catalogSearchView struct {
	CatalogID   uint
	Version     int // the catalog version searched
	Query       string
	Result      searchResult
	Error       string
	ShowVersion bool
	ShowID      bool
	ShowAlbum   bool
}

func (a *app) catalogPageFor(r *http.Request, c Catalog) catalogPage {
	q := r.URL.Query()
	return catalogPage{
		Catalog: c, Metadata: c.ImportMetadata(), Saved: q.Get("saved"), Built: q.Get("built") == "1",
		VariantChoices: variantDisplayChoices,
		Search:         catalogSearchView{CatalogID: c.ID},
	}
}

func (a *app) handleCatalog(w http.ResponseWriter, r *http.Request) {
	c, ok := a.ownCatalog(w, r)
	if !ok {
		return
	}
	page := a.catalogPageFor(r, c)
	if q := r.URL.Query().Get("q"); strings.TrimSpace(q) != "" && c.ActiveVersion != nil {
		page.Search = a.searchView(r, c, q)
	}
	a.render(w, r, http.StatusOK, "catalog.html", page)
}

func (a *app) handleCatalogSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := a.ownCatalog(w, r)
	if !ok {
		return
	}
	vd := r.PostFormValue("variant_display")
	valid := false
	for _, ch := range variantDisplayChoices {
		valid = valid || ch.Value == vd
	}
	if !valid {
		http.Error(w, "Variant display is not included in the list", http.StatusUnprocessableEntity)
		return
	}
	if err := a.db.Model(&c).Update("variant_display", vd).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/catalogs/%d?saved=settings", c.ID), http.StatusSeeOther)
}

// handleCatalogUpload stores the CSV in the blob store and queues a build.
// Nothing about the contents is checked here; the builder does that.
func (a *app) handleCatalogUpload(w http.ResponseWriter, r *http.Request) {
	c, ok := a.ownCatalog(w, r)
	if !ok {
		return
	}
	fail := func(status int, msg string) {
		page := a.catalogPageFor(r, c)
		page.UploadError = msg
		a.render(w, r, status, "catalog.html", page)
	}
	if !a.uploadLimiter.allow(strconv.FormatUint(uint64(c.UserID), 10)) {
		fail(http.StatusTooManyRequests, "You've uploaded 5 files in the last hour. Please try again later.")
		return
	}

	// The server's timeouts are sized for pages, not a 100 MB upload on a slow connection.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(uploadTimeout))
	rc.SetWriteDeadline(time.Now().Add(uploadTimeout))
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)

	path, size, sum, err := a.receiveUpload(r)
	if path != "" {
		defer os.Remove(path)
	}
	switch {
	case errors.Is(err, errNoFile):
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, errFileTooLarge):
		w.Header().Set("Connection", "close")
		fail(http.StatusRequestEntityTooLarge, err.Error())
		return
	case err != nil:
		a.serverError(w, r, err)
		return
	}

	if c.Status == catalogReady && sum == c.LastCSVSHA256 {
		http.Redirect(w, r, fmt.Sprintf("/catalogs/%d?saved=unchanged", c.ID), http.StatusSeeOther)
		return
	}

	v := CatalogVersion{CatalogID: c.ID, CSVSHA256: sum, CSVBytes: size}
	err = a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&CatalogVersion{}).Where("catalog_id = ?", c.ID).
			Select("COALESCE(MAX(version), 0) + 1").Scan(&v.Version).Error; err != nil {
			return err
		}
		return tx.Create(&v).Error
	})
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	f, err := os.Open(path)
	if err == nil {
		err = a.blobs.put(r.Context(), csvBlobKey(c.ID, v.Version), f)
		f.Close()
	}
	if err == nil {
		err = a.db.Model(&Catalog{}).Where("id = ?", c.ID).Updates(map[string]any{
			"status": catalogPending, "building_version": v.Version, "errors_json": "", "updated_at": a.now(),
		}).Error
	}
	if err != nil {
		a.db.Delete(&v)
		a.serverError(w, r, err)
		return
	}
	a.wakeBuilder()
	http.Redirect(w, r, fmt.Sprintf("/catalogs/%d", c.ID), http.StatusSeeOther)
}

// receiveUpload streams the "file" part of a multipart body to a temp file,
// hashing it on the way.
func (a *app) receiveUpload(r *http.Request) (path string, size int64, sum string, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return "", 0, "", errNoFile
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return "", 0, "", errNoFile
		}
		if err != nil {
			return "", 0, "", uploadErr(err)
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		return a.saveUpload(part)
	}
}

func (a *app) saveUpload(part *multipart.Part) (string, int64, string, error) {
	defer part.Close()
	tmp, err := os.CreateTemp(a.cfg.CatalogCacheDir, "upload-*.tmp")
	if err != nil {
		return "", 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(part, maxUploadBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxUploadBytes {
		err = errFileTooLarge
	}
	if err != nil {
		return tmp.Name(), 0, "", uploadErr(err)
	}
	return tmp.Name(), n, hex.EncodeToString(h.Sum(nil)), nil
}

func uploadErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return errFileTooLarge
	}
	return err
}

// handleCatalogStatus is polled while a build runs: 204 until it is done,
// then a full reload so the song count, search and messages all update.
func (a *app) handleCatalogStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := a.ownCatalog(w, r)
	if !ok {
		return
	}
	if c.Building() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("HX-Redirect", fmt.Sprintf("/catalogs/%d?built=1", c.ID))
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleCatalogSearch(w http.ResponseWriter, r *http.Request) {
	c, ok := a.ownCatalog(w, r)
	if !ok {
		return
	}
	page := catalogPage{Catalog: c, Search: catalogSearchView{CatalogID: c.ID}}
	if q := r.URL.Query().Get("q"); strings.TrimSpace(q) != "" && c.ActiveVersion != nil {
		page.Search = a.searchView(r, c, q)
	}
	a.renderBlock(w, r, http.StatusOK, "catalog.html", "search_results", page)
}

func (a *app) searchView(r *http.Request, c Catalog, q string) catalogSearchView {
	view := catalogSearchView{CatalogID: c.ID, Version: *c.ActiveVersion, Query: q}
	db, err := a.cache.open(r.Context(), c.ID, *c.ActiveVersion)
	if err == nil {
		start := time.Now()
		view.Result, err = searchCatalog(db, q, catalogSearchLimit, 0, r.URL.Query().Get("sort"))
		if d := time.Since(start); d > 500*time.Millisecond {
			a.logger.Info("slow catalog search", "catalog_id", c.ID, "ms", d.Milliseconds(),
				"sort", view.Result.Sort, "returned", len(view.Result.Songs))
		}
	}
	if err != nil {
		a.logger.Error("catalog search", "catalog_id", c.ID, "err", err)
		view.Error = "Something went wrong while searching. Please try again."
		return view
	}
	for _, s := range view.Result.Songs {
		view.ShowVersion = view.ShowVersion || (c.VariantDisplay == "version" && len(s.Versions) > 0)
		view.ShowID = view.ShowID || (c.VariantDisplay == "id" && len(s.ExternalIDs) > 0)
		view.ShowAlbum = view.ShowAlbum || s.Album != nil && *s.Album != ""
	}
	return view
}

func (a *app) handleCatalogDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := a.ownCatalog(w, r)
	if !ok {
		return
	}
	var live int64
	if err := a.db.Model(&Show{}).Where("catalog_id = ? AND status = ?", c.ID, showActive).Count(&live).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	if live > 0 {
		page := a.catalogPageFor(r, c)
		page.DeleteError = "End all active shows before deleting this catalog."
		a.render(w, r, http.StatusConflict, "catalog.html", page)
		return
	}
	if err := a.db.Delete(&c).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	a.removeCatalogData(r.Context(), c.ID)
	http.Redirect(w, r, "/catalogs?saved=deleted", http.StatusSeeOther)
}

// commas formats n with thousands separators, as the Rails pages did.
func commas(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + commas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
