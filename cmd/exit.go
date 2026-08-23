package cmd

import "errors"

// ExitCodeDiff は --exit-code 指定時に差分があったことを表す終了コード。
// terraform plan -detailed-exitcode と同じ割り当て (0 = 差分なし、1 = エラー、
// 2 = 差分あり) にする。こうすればエラーの 1 を変えずに済み、既存のスクリプトを
// 壊さない。
const ExitCodeDiff = 2

// errDiffFound は diff --exit-code で差分があったときに返す番兵。
var errDiffFound = errors.New("differences found")

// ExitCode は Execute が返したエラーに対応する終了コードを返す。
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errDiffFound):
		return ExitCodeDiff
	default:
		return 1
	}
}
