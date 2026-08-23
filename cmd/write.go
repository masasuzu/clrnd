package cmd

import (
	"fmt"
	"os"
	"path/filepath"
)

// privateFileMode は clrnd が生成するファイルの権限。render の出力には must_env で
// 展開した秘密が、init の出力には live サービスの平文の env[].value が入りうるので、
// 他ユーザから読めないようにする。
const privateFileMode os.FileMode = 0o600

// writeFilePrivate は data を path へ書き、書き終わったファイルの権限を必ず
// privateFileMode にする。
//
// os.WriteFile では足りない理由が 2 つある。perm は新規作成時にしか適用されないので、
// 既存の出力先が 0644 なら秘密を書いても 0644 のまま残る。そして既存ファイルは書き込み
// 前に truncate されるので、途中で失敗すると以前の正常な内容まで失う。
// 同じディレクトリの一時ファイルへ書ききってから rename することで、どちらも避ける。
//
// 「置き換えが原子的で、path が常に古い内容か新しい内容のどちらかになる」と言えるのは
// Unix の rename に限る。Windows の os.Rename は MoveFileEx(MOVEFILE_REPLACE_EXISTING)
// で、置換の原子性は OS が保証しないので、クラッシュ時の中間状態はありえる。それでも
// 一時ファイル経由にする意味は残る: 書き込みの失敗 (容量不足/I-O エラー) では出力先に
// 触れないままなので前の内容が残り、部分的に書けたファイルが表に出ることも無い。
// mode についても同じ線引きで、Windows のファイル権限は読み取り専用ビットに丸められる
// ため、0600 が「他ユーザから読めない」ことを意味するのは Unix だけ。
//
// Windows では出力先が他プロセスに開かれていると rename が失敗するが、これは失敗として
// 返るだけで、出力先の内容は壊れない。
func writeFilePrivate(path string, data []byte) error {
	// /dev/null やプロセス置換 (>(cmd)) のような通常ファイルでない出力先には、
	// 置き換えるべき中身も権限も無い。rename もできないのでそのまま書く。
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		if err := os.WriteFile(path, data, privateFileMode); err != nil {
			return fmt.Errorf("failed to write to %s: %w", path, err)
		}
		return nil
	}

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("failed to write to %s: %w", path, err)
	}
	tmp := f.Name()
	// 失敗して抜ける経路では一時ファイルを残さない。rename に成功したら tmp を空にする。
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if err := writeAndClose(f, data); err != nil {
		return fmt.Errorf("failed to write to %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("failed to write to %s: %w", path, err)
	}
	tmp = ""
	return nil
}

// writeAndClose は開いたファイルへ中身を書ききって閉じる。作成時の perm は umask で
// 削られるため、権限は明示的に設定し直して確定させる。
func writeAndClose(f *os.File, data []byte) error {
	defer func() { _ = f.Close() }()

	if err := f.Chmod(privateFileMode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	// 書いた内容がディスクに届く前に rename すると、クラッシュ時に空のファイルが残りうる。
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// writeFileExclusive は path がまだ無い場合にだけ作成して書く。存在確認と作成を
// 1 回の O_CREATE|O_EXCL で行うので、確認の後・書き込みの前に作られたファイルを
// 上書きしない (--force を渡していないのに既存ファイルが消えることを防ぐ)。
// 書き込みに失敗した場合は、自分が作ったファイルを消して何も残さない。
func writeFileExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists: pass --force to overwrite", path)
		}
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	if err := writeAndClose(f, data); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}
