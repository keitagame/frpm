package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Package represents a software package
type Package struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Architecture string            `json:"arch"`
	Description  string            `json:"description"`
	Dependencies []string          `json:"dependencies"`
	Conflicts    []string          `json:"conflicts"`
	Size         int64             `json:"size"`
	URL          string            `json:"url"`
	Checksum     string            `json:"checksum"`
	Signature    string            `json:"signature"`
	Files        []string          `json:"files"`
	Metadata     map[string]string `json:"metadata"`
}

// Repository represents a package repository
type Repository struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
	Trusted  bool   `json:"trusted"`
}

// Transaction represents an installation/removal transaction
type Transaction struct {
	ID        int64
	Type      string // install, remove, upgrade
	Packages  string // JSON array of package names
	Timestamp time.Time
	Success   bool
}

// PackageManager is the main package manager structure
type PackageManager struct {
	db          *sql.DB
	dbPath      string
	rootDir     string
	cacheDir    string
	repos       []Repository
	reposFile   string
	lockFile    string
}

// NewPackageManager creates a new package manager instance
func NewPackageManager(rootDir string) (*PackageManager, error) {
	pm := &PackageManager{
		rootDir:   rootDir,
		dbPath:    filepath.Join(rootDir, "var/lib/pkgmgr/packages.db"),
		cacheDir:  filepath.Join(rootDir, "var/cache/pkgmgr"),
		reposFile: filepath.Join(rootDir, "etc/pkgmgr/repositories.json"),
		lockFile:  filepath.Join(rootDir, "var/lib/pkgmgr/lock"),
	}

	// Create necessary directories
	dirs := []string{
		filepath.Dir(pm.dbPath),
		pm.cacheDir,
		filepath.Dir(pm.reposFile),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %v", dir, err)
		}
	}

	// Initialize database
	if err := pm.initDB(); err != nil {
		return nil, err
	}

	// Load repositories
	if err := pm.loadRepositories(); err != nil {
		return nil, err
	}

	return pm, nil
}

// initDB initializes the SQLite database
func (pm *PackageManager) initDB() error {
	db, err := sql.Open("sqlite3", pm.dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %v", err)
	}
	pm.db = db

	// Create tables
	schema := `
	CREATE TABLE IF NOT EXISTS installed_packages (
		name TEXT PRIMARY KEY,
		version TEXT NOT NULL,
		architecture TEXT NOT NULL,
		description TEXT,
		dependencies TEXT,
		conflicts TEXT,
		size INTEGER,
		install_date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		files TEXT
	);

	CREATE TABLE IF NOT EXISTS available_packages (
		name TEXT,
		version TEXT,
		repository TEXT,
		architecture TEXT,
		description TEXT,
		dependencies TEXT,
		conflicts TEXT,
		size INTEGER,
		url TEXT,
		checksum TEXT,
		signature TEXT,
		PRIMARY KEY (name, version, repository)
	);

	CREATE TABLE IF NOT EXISTS transactions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		type TEXT NOT NULL,
		packages TEXT NOT NULL,
		timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		success BOOLEAN
	);

	CREATE TABLE IF NOT EXISTS repositories (
		name TEXT PRIMARY KEY,
		url TEXT NOT NULL,
		priority INTEGER DEFAULT 0,
		enabled BOOLEAN DEFAULT 1,
		trusted BOOLEAN DEFAULT 0
	);

	CREATE INDEX IF NOT EXISTS idx_pkg_name ON available_packages(name);
	CREATE INDEX IF NOT EXISTS idx_trans_time ON transactions(timestamp);
	`

	_, err = pm.db.Exec(schema)
	return err
}

// loadRepositories loads repository configuration
func (pm *PackageManager) loadRepositories() error {
	data, err := os.ReadFile(pm.reposFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default repositories file
			pm.repos = []Repository{
				{Name: "main", URL: "https://repo.example.com/main", Priority: 10, Enabled: true, Trusted: true},
			}
			return pm.saveRepositories()
		}
		return err
	}

	return json.Unmarshal(data, &pm.repos)
}

// saveRepositories saves repository configuration
func (pm *PackageManager) saveRepositories() error {
	data, err := json.MarshalIndent(pm.repos, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pm.reposFile, data, 0644)
}

// AddRepository adds a new repository
func (pm *PackageManager) AddRepository(name, url string, priority int, trusted bool) error {
	for i, repo := range pm.repos {
		if repo.Name == name {
			pm.repos[i].URL = url
			pm.repos[i].Priority = priority
			pm.repos[i].Trusted = trusted
			return pm.saveRepositories()
		}
	}

	pm.repos = append(pm.repos, Repository{
		Name:     name,
		URL:      url,
		Priority: priority,
		Enabled:  true,
		Trusted:  trusted,
	})

	return pm.saveRepositories()
}

// RemoveRepository removes a repository
func (pm *PackageManager) RemoveRepository(name string) error {
	for i, repo := range pm.repos {
		if repo.Name == name {
			pm.repos = append(pm.repos[:i], pm.repos[i+1:]...)
			return pm.saveRepositories()
		}
	}
	return fmt.Errorf("repository %s not found", name)
}

// UpdateRepositories updates package lists from all enabled repositories
func (pm *PackageManager) UpdateRepositories() error {
	fmt.Println("Updating package lists...")

	for _, repo := range pm.repos {
		if !repo.Enabled {
			continue
		}

		fmt.Printf("Fetching %s...\n", repo.Name)

		// Download repository index
		resp, err := http.Get(repo.URL + "/packages.json")
		if err != nil {
			fmt.Printf("Warning: failed to fetch %s: %v\n", repo.Name, err)
			continue
		}
		defer resp.Body.Close()

		var packages []Package
		if err := json.NewDecoder(resp.Body).Decode(&packages); err != nil {
			fmt.Printf("Warning: failed to parse %s: %v\n", repo.Name, err)
			continue
		}

		// Update database
		tx, err := pm.db.Begin()
		if err != nil {
			return err
		}

		stmt, err := tx.Prepare(`
			INSERT OR REPLACE INTO available_packages 
			(name, version, repository, architecture, description, dependencies, conflicts, size, url, checksum, signature)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`)
		if err != nil {
			tx.Rollback()
			return err
		}

		for _, pkg := range packages {
			deps, _ := json.Marshal(pkg.Dependencies)
			confs, _ := json.Marshal(pkg.Conflicts)

			_, err := stmt.Exec(
				pkg.Name, pkg.Version, repo.Name, pkg.Architecture,
				pkg.Description, string(deps), string(confs),
				pkg.Size, pkg.URL, pkg.Checksum, pkg.Signature,
			)
			if err != nil {
				stmt.Close()
				tx.Rollback()
				return err
			}
		}

		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}

		fmt.Printf("Updated %s: %d packages\n", repo.Name, len(packages))
	}

	return nil
}

// Search searches for packages matching the query
func (pm *PackageManager) Search(query string) ([]Package, error) {
	rows, err := pm.db.Query(`
		SELECT DISTINCT name, version, repository, architecture, description, size
		FROM available_packages
		WHERE name LIKE ? OR description LIKE ?
		ORDER BY name, version DESC
	`, "%"+query+"%", "%"+query+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var packages []Package
	for rows.Next() {
		var pkg Package
		var repo string
		err := rows.Scan(&pkg.Name, &pkg.Version, &repo, &pkg.Architecture, &pkg.Description, &pkg.Size)
		if err != nil {
			return nil, err
		}
		packages = append(packages, pkg)
	}

	return packages, nil
}

// ResolveDependencies resolves package dependencies recursively
func (pm *PackageManager) ResolveDependencies(pkgName string) ([]string, error) {
	resolved := make(map[string]bool)
	var resolve func(string) error

	resolve = func(name string) error {
		if resolved[name] {
			return nil
		}

		// Check if already installed
		var installed int
		err := pm.db.QueryRow("SELECT COUNT(*) FROM installed_packages WHERE name = ?", name).Scan(&installed)
		if err != nil {
			return err
		}
		if installed > 0 {
			resolved[name] = true
			return nil
		}

		// Get package info
		var depsJSON string
		err = pm.db.QueryRow(`
			SELECT dependencies FROM available_packages 
			WHERE name = ? 
			ORDER BY version DESC LIMIT 1
		`, name).Scan(&depsJSON)
		if err != nil {
			return fmt.Errorf("package %s not found", name)
		}

		resolved[name] = true

		// Resolve dependencies
		var deps []string
		if depsJSON != "" {
			json.Unmarshal([]byte(depsJSON), &deps)
		}

		for _, dep := range deps {
			if err := resolve(dep); err != nil {
				return err
			}
		}

		return nil
	}

	if err := resolve(pkgName); err != nil {
		return nil, err
	}

	var result []string
	for name := range resolved {
		result = append(result, name)
	}

	return result, nil
}

// CheckConflicts checks for package conflicts
func (pm *PackageManager) CheckConflicts(packages []string) error {
	conflicts := make(map[string][]string)

	for _, pkg := range packages {
		var conflictsJSON string
		err := pm.db.QueryRow(`
			SELECT conflicts FROM available_packages 
			WHERE name = ? 
			ORDER BY version DESC LIMIT 1
		`, pkg).Scan(&conflictsJSON)
		if err != nil {
			continue
		}

		var pkgConflicts []string
		if conflictsJSON != "" {
			json.Unmarshal([]byte(conflictsJSON), &pkgConflicts)
		}

		for _, conflict := range pkgConflicts {
			conflicts[pkg] = append(conflicts[pkg], conflict)
		}
	}

	// Check if any conflicts exist in the install list or installed packages
	for pkg, conflictList := range conflicts {
		for _, conflict := range conflictList {
			// Check in install list
			for _, installPkg := range packages {
				if installPkg == conflict {
					return fmt.Errorf("conflict: %s conflicts with %s", pkg, conflict)
				}
			}

			// Check in installed packages
			var installed int
			pm.db.QueryRow("SELECT COUNT(*) FROM installed_packages WHERE name = ?", conflict).Scan(&installed)
			if installed > 0 {
				return fmt.Errorf("conflict: %s conflicts with installed package %s", pkg, conflict)
			}
		}
	}

	return nil
}

// Install installs a package and its dependencies
func (pm *PackageManager) Install(pkgName string) error {
	fmt.Printf("Resolving dependencies for %s...\n", pkgName)

	// Resolve dependencies
	packages, err := pm.ResolveDependencies(pkgName)
	if err != nil {
		return err
	}

	// Check conflicts
	if err := pm.CheckConflicts(packages); err != nil {
		return err
	}

	fmt.Printf("Packages to install: %s\n", strings.Join(packages, ", "))

	// Start transaction
	txID, err := pm.beginTransaction("install", packages)
	if err != nil {
		return err
	}

	success := true
	for _, pkg := range packages {
		if err := pm.installPackage(pkg); err != nil {
			fmt.Printf("Error installing %s: %v\n", pkg, err)
			success = false
			break
		}
	}

	pm.endTransaction(txID, success)

	if !success {
		return fmt.Errorf("installation failed")
	}

	fmt.Println("Installation completed successfully")
	return nil
}

// installPackage installs a single package
func (pm *PackageManager) installPackage(pkgName string) error {
	// Get package info
	var pkg Package
	var depsJSON, conflictsJSON, filesJSON string
	err := pm.db.QueryRow(`
		SELECT name, version, architecture, description, dependencies, conflicts, size, url, checksum
		FROM available_packages 
		WHERE name = ? 
		ORDER BY version DESC LIMIT 1
	`, pkgName).Scan(
		&pkg.Name, &pkg.Version, &pkg.Architecture, &pkg.Description,
		&depsJSON, &conflictsJSON, &pkg.Size, &pkg.URL, &pkg.Checksum,
	)
	if err != nil {
		return err
	}

	fmt.Printf("Installing %s %s...\n", pkg.Name, pkg.Version)

	// Download package (simulated)
	// In real implementation, download from pkg.URL and verify checksum

	// Record installation
	_, err = pm.db.Exec(`
		INSERT OR REPLACE INTO installed_packages 
		(name, version, architecture, description, dependencies, conflicts, size, files)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, pkg.Name, pkg.Version, pkg.Architecture, pkg.Description,
		depsJSON, conflictsJSON, pkg.Size, filesJSON)

	return err
}

// Remove removes a package
func (pm *PackageManager) Remove(pkgName string) error {
	// Check if package is installed
	var installed int
	err := pm.db.QueryRow("SELECT COUNT(*) FROM installed_packages WHERE name = ?", pkgName).Scan(&installed)
	if err != nil {
		return err
	}
	if installed == 0 {
		return fmt.Errorf("package %s is not installed", pkgName)
	}

	// Check if other packages depend on this
	rows, err := pm.db.Query(`
		SELECT name FROM installed_packages 
		WHERE dependencies LIKE ?
	`, "%"+pkgName+"%")
	if err != nil {
		return err
	}
	defer rows.Close()

	var dependents []string
	for rows.Next() {
		var dep string
		rows.Scan(&dep)
		dependents = append(dependents, dep)
	}

	if len(dependents) > 0 {
		return fmt.Errorf("cannot remove %s: required by %s", pkgName, strings.Join(dependents, ", "))
	}

	fmt.Printf("Removing %s...\n", pkgName)

	// Start transaction
	txID, err := pm.beginTransaction("remove", []string{pkgName})
	if err != nil {
		return err
	}

	// Remove from database
	_, err = pm.db.Exec("DELETE FROM installed_packages WHERE name = ?", pkgName)

	pm.endTransaction(txID, err == nil)

	if err != nil {
		return err
	}

	fmt.Println("Package removed successfully")
	return nil
}

// Upgrade upgrades an installed package
func (pm *PackageManager) Upgrade(pkgName string) error {
	// Check current version
	var currentVersion string
	err := pm.db.QueryRow("SELECT version FROM installed_packages WHERE name = ?", pkgName).Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("package %s is not installed", pkgName)
	}

	// Check available version
	var availableVersion string
	err = pm.db.QueryRow(`
		SELECT version FROM available_packages 
		WHERE name = ? 
		ORDER BY version DESC LIMIT 1
	`, pkgName).Scan(&availableVersion)
	if err != nil {
		return fmt.Errorf("no updates available for %s", pkgName)
	}

	if currentVersion >= availableVersion {
		fmt.Printf("%s is already at the latest version (%s)\n", pkgName, currentVersion)
		return nil
	}

	fmt.Printf("Upgrading %s from %s to %s...\n", pkgName, currentVersion, availableVersion)

	return pm.installPackage(pkgName)
}

// ListInstalled lists all installed packages
func (pm *PackageManager) ListInstalled() error {
	rows, err := pm.db.Query(`
		SELECT name, version, size, install_date 
		FROM installed_packages 
		ORDER BY name
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println("Installed packages:")
	fmt.Println("-------------------")
	for rows.Next() {
		var name, version, installDate string
		var size int64
		rows.Scan(&name, &version, &size, &installDate)
		fmt.Printf("%-30s %-15s %10d KB  %s\n", name, version, size/1024, installDate[:10])
	}

	return nil
}

// ShowHistory shows transaction history
func (pm *PackageManager) ShowHistory(limit int) error {
	rows, err := pm.db.Query(`
		SELECT id, type, packages, timestamp, success 
		FROM transactions 
		ORDER BY timestamp DESC 
		LIMIT ?
	`, limit)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println("Transaction history:")
	fmt.Println("--------------------")
	for rows.Next() {
		var tx Transaction
		var packagesJSON string
		rows.Scan(&tx.ID, &tx.Type, &packagesJSON, &tx.Timestamp, &tx.Success)

		status := "SUCCESS"
		if !tx.Success {
			status = "FAILED"
		}

		fmt.Printf("[%d] %s - %s - %s - %s\n",
			tx.ID, tx.Timestamp.Format("2006-01-02 15:04:05"),
			tx.Type, packagesJSON, status)
	}

	return nil
}

// Rollback rolls back to a previous transaction
func (pm *PackageManager) Rollback(txID int64) error {
	var txType, packagesJSON string
	err := pm.db.QueryRow(`
		SELECT type, packages FROM transactions WHERE id = ?
	`, txID).Scan(&txType, &packagesJSON)
	if err != nil {
		return fmt.Errorf("transaction %d not found", txID)
	}

	var packages []string
	json.Unmarshal([]byte(packagesJSON), &packages)

	fmt.Printf("Rolling back transaction %d (%s)...\n", txID, txType)

	// Reverse the operation
	switch txType {
	case "install":
		for _, pkg := range packages {
			pm.Remove(pkg)
		}
	case "remove":
		for _, pkg := range packages {
			pm.Install(pkg)
		}
	}

	return nil
}

// Clean removes cached package files
func (pm *PackageManager) Clean() error {
	fmt.Println("Cleaning package cache...")

	err := filepath.Walk(pm.cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			os.Remove(path)
		}
		return nil
	})

	if err != nil {
		return err
	}

	fmt.Println("Cache cleaned")
	return nil
}

// beginTransaction starts a new transaction record
func (pm *PackageManager) beginTransaction(txType string, packages []string) (int64, error) {
	packagesJSON, _ := json.Marshal(packages)
	result, err := pm.db.Exec(`
		INSERT INTO transactions (type, packages, success) 
		VALUES (?, ?, 0)
	`, txType, string(packagesJSON))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// endTransaction marks a transaction as complete
func (pm *PackageManager) endTransaction(txID int64, success bool) {
	pm.db.Exec("UPDATE transactions SET success = ? WHERE id = ?", success, txID)
}

// VerifyChecksum verifies a file's checksum
func (pm *PackageManager) VerifyChecksum(filePath, expectedChecksum string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}

	actualChecksum := hex.EncodeToString(h.Sum(nil))
	if actualChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
	}

	return nil
}

// Close closes the package manager and database connection
func (pm *PackageManager) Close() error {
	if pm.db != nil {
		return pm.db.Close()
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	pm, err := NewPackageManager("/")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	command := os.Args[1]

	switch command {
	case "install", "i":
		if len(os.Args) < 3 {
			fmt.Println("Usage: pkgmgr install <package>")
			os.Exit(1)
		}
		err = pm.Install(os.Args[2])

	case "remove", "r":
		if len(os.Args) < 3 {
			fmt.Println("Usage: pkgmgr remove <package>")
			os.Exit(1)
		}
		err = pm.Remove(os.Args[2])

	case "upgrade", "u":
		if len(os.Args) < 3 {
			fmt.Println("Usage: pkgmgr upgrade <package>")
			os.Exit(1)
		}
		err = pm.Upgrade(os.Args[2])

	case "update":
		err = pm.UpdateRepositories()

	case "search", "s":
		if len(os.Args) < 3 {
			fmt.Println("Usage: pkgmgr search <query>")
			os.Exit(1)
		}
		packages, err := pm.Search(os.Args[2])
		if err == nil {
			for _, pkg := range packages {
				fmt.Printf("%-30s %-15s %s\n", pkg.Name, pkg.Version, pkg.Description)
			}
		}

	case "list":
		err = pm.ListInstalled()

	case "history":
		limit := 20
		err = pm.ShowHistory(limit)

	case "rollback":
		if len(os.Args) < 3 {
			fmt.Println("Usage: pkgmgr rollback <transaction_id>")
			os.Exit(1)
		}
		var txID int64
		fmt.Sscanf(os.Args[2], "%d", &txID)
		err = pm.Rollback(txID)

	case "clean":
		err = pm.Clean()

	case "repo-add":
		if len(os.Args) < 4 {
			fmt.Println("Usage: pkgmgr repo-add <name> <url> [priority] [trusted]")
			os.Exit(1)
		}
		priority := 0
		trusted := false
		if len(os.Args) > 4 {
			fmt.Sscanf(os.Args[4], "%d", &priority)
		}
		if len(os.Args) > 5 {
			trusted = os.Args[5] == "true"
		}
		err = pm.AddRepository(os.Args[2], os.Args[3], priority, trusted)

	case "repo-remove":
		if len(os.Args) < 3 {
			fmt.Println("Usage: pkgmgr repo-remove <name>")
			os.Exit(1)
		}
		err = pm.RemoveRepository(os.Args[2])

	default:
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Package Manager - A modern package management system

Usage:
  pkgmgr <command> [arguments]

Commands:
  install, i <package>           Install a package and its dependencies
  remove, r <package>            Remove a package
  upgrade, u <package>           Upgrade a package to the latest version
  update                         Update repository package lists
  search, s <query>              Search for packages
  list                           List installed packages
  history                        Show transaction history
  rollback <transaction_id>      Rollback to a previous transaction
  clean                          Clean package cache
  repo-add <name> <url>          Add a new repository
  repo-remove <name>             Remove a repository

Examples:
  pkgmgr install nginx
  pkgmgr search http
  pkgmgr update
  pkgmgr list`)
}
