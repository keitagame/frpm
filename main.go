package main

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

type PackageManager struct {
	db          *sql.DB
	buildDir    string
	installRoot string
}

type Package struct {
	Name        string
	Version     string
	Release     string
	Arch        string
	Source      []string
	Depends     []string
	MakeDepends []string
	PrepareCmd  string
	BuildCmd    string
	PackageCmd  string
}

func NewPackageManager(dbPath, buildDir, installRoot string) (*PackageManager, error) {
	dbDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("DBディレクトリの作成に失敗: %v", err)
	}

	if err := os.MkdirAll(buildDir, 0755); err != nil {
		return nil, fmt.Errorf("ビルドディレクトリの作成に失敗: %v", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	pm := &PackageManager{
		db:          db,
		buildDir:    buildDir,
		installRoot: installRoot,
	}

	if err := pm.initDB(); err != nil {
		return nil, err
	}

	return pm, nil
}

func (pm *PackageManager) initDB() error {
	schema := `
	CREATE TABLE IF NOT EXISTS packages (
		name TEXT PRIMARY KEY,
		version TEXT NOT NULL,
		release TEXT NOT NULL,
		arch TEXT NOT NULL,
		installed INTEGER DEFAULT 0,
		installed_at TIMESTAMP
	);
	
	CREATE TABLE IF NOT EXISTS sources (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		package_name TEXT NOT NULL,
		url TEXT NOT NULL,
		FOREIGN KEY (package_name) REFERENCES packages(name)
	);
	
	CREATE TABLE IF NOT EXISTS dependencies (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		package_name TEXT NOT NULL,
		depends_on TEXT NOT NULL,
		FOREIGN KEY (package_name) REFERENCES packages(name)
	);
	`
	_, err := pm.db.Exec(schema)
	return err
}

func (pm *PackageManager) ParsePKGBUILD(path string) (*Package, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	pkg := &Package{}
	text := string(content)

	// 基本変数の抽出
	pkg.Name = extractSimpleVar(text, "pkgname")
	pkg.Version = extractSimpleVar(text, "pkgver")
	pkg.Release = extractSimpleVar(text, "pkgrel")
	pkg.Arch = extractSimpleVar(text, "arch")

	fmt.Printf("デバッグ: pkgname=%s, pkgver=%s, pkgrel=%s\n", pkg.Name, pkg.Version, pkg.Release)

	// 配列の抽出
	pkg.Source = extractArrayVar(text, "source")
	pkg.Depends = extractArrayVar(text, "depends")
	pkg.MakeDepends = extractArrayVar(text, "makedepends")

	fmt.Printf("デバッグ: source数=%d, depends数=%d\n", len(pkg.Source), len(pkg.Depends))

	// 関数の抽出
	pkg.PrepareCmd = extractBashFunction(text, "prepare")
	pkg.BuildCmd = extractBashFunction(text, "build")
	pkg.PackageCmd = extractBashFunction(text, "package")

	if pkg.PrepareCmd != "" {
		fmt.Printf("デバッグ: prepare関数が見つかりました（%d文字）\n", len(pkg.PrepareCmd))
	}
	if pkg.BuildCmd != "" {
		fmt.Printf("デバッグ: build関数が見つかりました（%d文字）\n", len(pkg.BuildCmd))
	}
	if pkg.PackageCmd != "" {
		fmt.Printf("デバッグ: package関数が見つかりました（%d文字）\n", len(pkg.PackageCmd))
	}

	if pkg.Name == "" {
		return nil, fmt.Errorf("pkgnameが見つかりません")
	}

	return pkg, nil
}

func extractSimpleVar(content, varName string) string {
	re := regexp.MustCompile(`(?m)^\s*` + varName + `=([^\n]+)`)
	matches := re.FindStringSubmatch(content)
	if len(matches) < 2 {
		return ""
	}
	val := strings.TrimSpace(matches[1])
	val = strings.Trim(val, `"'()`)
	return val
}

func extractArrayVar(content, varName string) []string {
	result := []string{}

	// 単一行配列: var=(item1 item2)
	re := regexp.MustCompile(`(?m)^\s*` + varName + `=\(([^)]+)\)`)
	matches := re.FindStringSubmatch(content)
	if len(matches) >= 2 {
		items := strings.Fields(matches[1])
		for _, item := range items {
			item = strings.Trim(item, `"'`)
			if item != "" && !strings.HasPrefix(item, "#") {
				result = append(result, item)
			}
		}
		return result
	}

	// 複数行配列
	re = regexp.MustCompile(`(?s)^\s*` + varName + `=\(\s*\n(.*?)\n\s*\)`)
	matches = re.FindStringSubmatch(content)
	if len(matches) >= 2 {
		lines := strings.Split(matches[1], "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.Trim(line, `"'`)
			result = append(result, line)
		}
	}

	return result
}

func extractBashFunction(content, funcName string) string {
	// パターン1: function_name() { ... }
	re1 := regexp.MustCompile(`(?ms)^` + funcName + `\(\)\s*\{(.*?)^}`)
	matches := re1.FindStringSubmatch(content)
	if len(matches) >= 2 {
		body := matches[1]
		// 最初と最後の空行を削除
		body = strings.TrimSpace(body)
		fmt.Printf("デバッグ: %s()関数を抽出しました（パターン1、%d文字）\n", funcName, len(body))
		return body
	}

	// パターン2: function function_name { ... }
	re2 := regexp.MustCompile(`(?ms)^function\s+` + funcName + `\s*\{(.*?)^}`)
	matches = re2.FindStringSubmatch(content)
	if len(matches) >= 2 {
		body := matches[1]
		body = strings.TrimSpace(body)
		fmt.Printf("デバッグ: %s()関数を抽出しました（パターン2、%d文字）\n", funcName, len(body))
		return body
	}

	// パターン3: より緩い検索（インデント対応）
	lines := strings.Split(content, "\n")
	inFunction := false
	braceCount := 0
	var functionBody []string
	
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		
		// 関数の開始を検出
		if !inFunction && (trimmed == funcName+"() {" || strings.HasPrefix(trimmed, funcName+"()")) {
			inFunction = true
			braceCount = strings.Count(line, "{") - strings.Count(line, "}")
			fmt.Printf("デバッグ: %s()関数の開始を検出（行%d）\n", funcName, i+1)
			continue
		}
		
		if inFunction {
			braceCount += strings.Count(line, "{") - strings.Count(line, "}")
			
			if braceCount == 0 && trimmed == "}" {
				// 関数の終了
				body := strings.Join(functionBody, "\n")
				body = strings.TrimSpace(body)
				fmt.Printf("デバッグ: %s()関数を抽出しました（パターン3、%d文字、%d行）\n", funcName, len(body), len(functionBody))
				return body
			}
			
			functionBody = append(functionBody, line)
		}
	}
	
	fmt.Printf("デバッグ: %s()関数が見つかりませんでした\n", funcName)
	return ""
}

func (pm *PackageManager) Install(pkgbuildPath string) error {
	pkg, err := pm.ParsePKGBUILD(pkgbuildPath)
	if err != nil {
		return fmt.Errorf("PKGBUILDの解析に失敗: %v", err)
	}

	fmt.Printf("パッケージをインストール: %s-%s-%s\n", pkg.Name, pkg.Version, pkg.Release)
	fmt.Printf("依存関係: %v\n", pkg.Depends)
	fmt.Printf("ビルド依存: %v\n", pkg.MakeDepends)

	// ビルドディレクトリ作成
	pkgBuildDir := filepath.Join(pm.buildDir, pkg.Name)
	os.RemoveAll(pkgBuildDir)
	os.MkdirAll(pkgBuildDir, 0755)

	// PKGBUILDをコピー
	srcPkgbuild := pkgbuildPath
	dstPkgbuild := filepath.Join(pkgBuildDir, "PKGBUILD")
	if err := copyFile(srcPkgbuild, dstPkgbuild); err != nil {
		return fmt.Errorf("PKGBUILDのコピーに失敗: %v", err)
	}

	// 追加ファイルをコピー（.patchや.desktopなど）
	pkgbuildDir := filepath.Dir(pkgbuildPath)
	extraFiles, _ := filepath.Glob(filepath.Join(pkgbuildDir, "*"))
	for _, f := range extraFiles {
		if f != pkgbuildPath {
			base := filepath.Base(f)
			copyFile(f, filepath.Join(pkgBuildDir, base))
		}
	}

	// ソースダウンロード
	fmt.Println("\n==> ソースを取得中...")
	for _, src := range pkg.Source {
		// URLでない場合はスキップ（ローカルファイルとして扱う）
		if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
			continue
		}
		
		// {,.asc} のような表記を展開
		if strings.Contains(src, "{") {
			baseSrc := strings.Split(src, "{")[0]
			if err := pm.downloadSource(baseSrc, pkgBuildDir); err != nil {
				fmt.Printf("警告: %sのダウンロードに失敗: %v\n", baseSrc, err)
			}
		} else {
			if err := pm.downloadSource(src, pkgBuildDir); err != nil {
				fmt.Printf("警告: %sのダウンロードに失敗: %v\n", src, err)
			}
		}
	}

	// prepare実行
	if pkg.PrepareCmd != "" {
		fmt.Println("\n==> prepare()を実行中...")
		if err := pm.runPhase("prepare", pkg.PrepareCmd, pkgBuildDir, pkg); err != nil {
			return fmt.Errorf("prepareに失敗: %v", err)
		}
	} else {
		fmt.Println("\n==> prepare()関数なし、スキップ")
	}

	// build実行
	if pkg.BuildCmd != "" {
		fmt.Println("\n==> build()を実行中...")
		if err := pm.runPhase("build", pkg.BuildCmd, pkgBuildDir, pkg); err != nil {
			return fmt.Errorf("buildに失敗: %v", err)
		}
	} else {
		fmt.Println("\n==> build()関数なし、スキップ")
	}

	// package実行
	if pkg.PackageCmd != "" {
		fmt.Println("\n==> package()を実行中...")
		if err := pm.runPhase("package", pkg.PackageCmd, pkgBuildDir, pkg); err != nil {
			return fmt.Errorf("packageに失敗: %v", err)
		}
	} else {
		fmt.Println("\n==> package()関数なし、スキップ")
	}

	// pkgdirの内容をインストール
	pkgDir := filepath.Join(pkgBuildDir, "pkg")
	if _, err := os.Stat(pkgDir); err == nil {
		fmt.Println("\n==> ファイルをインストール中...")
		if err := pm.installFiles(pkgDir); err != nil {
			return fmt.Errorf("ファイルのインストールに失敗: %v", err)
		}
	}

	// DBに登録
	if err := pm.registerPackage(pkg); err != nil {
		return fmt.Errorf("パッケージの登録に失敗: %v", err)
	}

	fmt.Printf("\n==> パッケージ %s のインストールが完了しました\n", pkg.Name)
	return nil
}

func (pm *PackageManager) runPhase(phase, cmd, workDir string, pkg *Package) error {
	srcDir := filepath.Join(workDir, "src")
	pkgDir := filepath.Join(workDir, "pkg")
	os.MkdirAll(srcDir, 0755)
	os.MkdirAll(pkgDir, 0755)

	// makepkgの環境変数を設定
	env := os.Environ()
	env = append(env,
		fmt.Sprintf("srcdir=%s", srcDir),
		fmt.Sprintf("pkgdir=%s", pkgDir),
		fmt.Sprintf("pkgname=%s", pkg.Name),
		fmt.Sprintf("pkgver=%s", pkg.Version),
		fmt.Sprintf("pkgrel=%s", pkg.Release),
	)

	// PKGBUILDをsourceしてから関数を実行
	script := fmt.Sprintf(`
set -e
cd "%s"
source PKGBUILD
%s() {
%s
}
echo "==> %s()を実行します..."
%s
`, workDir, phase, cmd, phase, phase)

	fmt.Printf("デバッグ: 実行するスクリプト:\n%s\n", script)

	cmdExec := exec.Command("bash", "-c", script)
	cmdExec.Env = env
	cmdExec.Stdout = os.Stdout
	cmdExec.Stderr = os.Stderr

	return cmdExec.Run()
}

func (pm *PackageManager) downloadSource(url, destDir string) error {
	fmt.Printf("  -> ダウンロード中: %s\n", url)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	filename := filepath.Base(url)
	if idx := strings.Index(filename, "?"); idx > 0 {
		filename = filename[:idx]
	}

	destPath := filepath.Join(destDir, filename)

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func (pm *PackageManager) installFiles(pkgDir string) error {
	return filepath.Walk(pkgDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(pkgDir, path)
		if relPath == "." {
			return nil
		}

		destPath := filepath.Join(pm.installRoot, relPath)

		if info.IsDir() {
			return os.MkdirAll(destPath, info.Mode())
		}

		return copyFile(path, destPath)
	})
}

func copyFile(src, dst string) error {
	os.MkdirAll(filepath.Dir(dst), 0755)

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	srcInfo, _ := srcFile.Stat()
	return os.Chmod(dst, srcInfo.Mode())
}

func (pm *PackageManager) registerPackage(pkg *Package) error {
	tx, err := pm.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT OR REPLACE INTO packages (name, version, release, arch, installed, installed_at)
		VALUES (?, ?, ?, ?, 1, CURRENT_TIMESTAMP)
	`, pkg.Name, pkg.Version, pkg.Release, pkg.Arch)
	if err != nil {
		return err
	}

	for _, src := range pkg.Source {
		_, err = tx.Exec(`
			INSERT INTO sources (package_name, url) VALUES (?, ?)
		`, pkg.Name, src)
		if err != nil {
			return err
		}
	}

	for _, dep := range pkg.Depends {
		_, err = tx.Exec(`
			INSERT INTO dependencies (package_name, depends_on) VALUES (?, ?)
		`, pkg.Name, dep)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (pm *PackageManager) isInstalled(pkgName string) bool {
	var installed int
	err := pm.db.QueryRow(`
		SELECT installed FROM packages WHERE name = ?
	`, pkgName).Scan(&installed)
	return err == nil && installed == 1
}

func (pm *PackageManager) ListInstalled() error {
	rows, err := pm.db.Query(`
		SELECT name, version, release, installed_at 
		FROM packages 
		WHERE installed = 1
		ORDER BY name
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println("インストール済みパッケージ:")
	fmt.Println("----------------------------------------")
	count := 0
	for rows.Next() {
		var name, version, release, installedAt string
		if err := rows.Scan(&name, &version, &release, &installedAt); err != nil {
			return err
		}
		fmt.Printf("%s %s-%s (インストール日時: %s)\n", name, version, release, installedAt)
		count++
	}

	if count == 0 {
		fmt.Println("(なし)")
	}

	return rows.Err()
}

func (pm *PackageManager) Info(pkgName string) error {
	var name, version, release, arch, installedAt string
	err := pm.db.QueryRow(`
		SELECT name, version, release, arch, installed_at 
		FROM packages 
		WHERE name = ?
	`, pkgName).Scan(&name, &version, &release, &arch, &installedAt)

	if err == sql.ErrNoRows {
		fmt.Printf("パッケージ %s はインストールされていません\n", pkgName)
		return nil
	}
	if err != nil {
		return err
	}

	fmt.Printf("パッケージ名: %s\n", name)
	fmt.Printf("バージョン: %s-%s\n", version, release)
	fmt.Printf("アーキテクチャ: %s\n", arch)
	fmt.Printf("インストール日時: %s\n", installedAt)

	// 依存関係
	rows, err := pm.db.Query(`
		SELECT depends_on FROM dependencies WHERE package_name = ?
	`, pkgName)
	if err == nil {
		defer rows.Close()
		fmt.Print("依存関係: ")
		deps := []string{}
		for rows.Next() {
			var dep string
			rows.Scan(&dep)
			deps = append(deps, dep)
		}
		if len(deps) > 0 {
			fmt.Println(strings.Join(deps, ", "))
		} else {
			fmt.Println("(なし)")
		}
	}

	return nil
}

func (pm *PackageManager) Close() error {
	return pm.db.Close()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("使用方法:")
		fmt.Println("  install <PKGBUILD_PATH> - パッケージをインストール")
		fmt.Println("  list                    - インストール済みパッケージを表示")
		fmt.Println("  info <PKG_NAME>         - パッケージ情報を表示")
		os.Exit(1)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ホームディレクトリの取得エラー: %v\n", err)
		os.Exit(1)
	}

	pm, err := NewPackageManager(
		filepath.Join(homeDir, ".local/share/gopkg/packages.db"),
		filepath.Join(homeDir, ".cache/gopkg-build"),
		filepath.Join(homeDir, ".local"),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初期化エラー: %v\n", err)
		os.Exit(1)
	}
	defer pm.Close()

	cmd := os.Args[1]
	switch cmd {
	case "install":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "エラー: PKGBUILDのパスを指定してください")
			os.Exit(1)
		}
		if err := pm.Install(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "エラー: %v\n", err)
			os.Exit(1)
		}
	case "list":
		if err := pm.ListInstalled(); err != nil {
			fmt.Fprintf(os.Stderr, "エラー: %v\n", err)
			os.Exit(1)
		}
	case "info":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "エラー: パッケージ名を指定してください")
			os.Exit(1)
		}
		if err := pm.Info(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "エラー: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "不明なコマンド: %s\n", cmd)
		os.Exit(1)
	}
}
