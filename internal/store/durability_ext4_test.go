package store

// durability_ext4_test.go — 在真實磁碟（ext4）上量測 fsync 契約。
//
// ★ 這個檔案存在的理由，是既有的持久性測試全部跑在 tmpfs 上。
//
//	store_test.go 的 tempDir() helper 明確使用 $XDG_RUNTIME_DIR（本機 =
//	/run/user/1000，tmpfs）。而 tmpfs 的 fsync 是 no-op —— 於是「測試通過」
//	與「fsync 根本沒被呼叫」在 tmpfs 上共用同一個綠燈。那個綠燈證明的是
//	append→apply 的順序，不是斷電後資料還在。
//
// ★ 誠實的邊界，寫在最前面而不是藏在報告裡：
//
//	使用者空間的測試「無法」證明斷電後資料還在。SIGKILL 不會清掉 page
//	cache —— 檔案內容由 kernel 保管，殺掉行程之後另開一個行程照樣讀得到
//	沒有 fsync 過的位元組。所以任何「殺 process 再 replay」的測試，對
//	fsync 有沒有被呼叫「零判別力」：兩種寫法都會通過。
//
//	要真的證明斷電語意，需要 dm-flakey / dm_log_writes（本機 WSL2 kernel
//	5.15.167.4-microsoft-standard-WSL2+ 沒有這兩個模組）或真的拔電。
//
//	所以本檔改為證明「可在使用者空間被證明的那一半」，且每一條都自帶控制組：
//
//	  1. 這個目錄真的在 ext4 上（且探針對 tmpfs 回不同答案 → 有判別力）
//	  2. append() 真的對 log 檔發出 fsync 系統呼叫（strace 計數）
//	  3. 那個 fsync 在 ext4 上有真實成本（時間，相對 tmpfs 自我校準）
//	  4. 崩潰→replay 的順序契約在 ext4 上依然成立（順序，非斷電）
//
//	第 1~3 條是既有測試「量不到」的東西；第 4 條是既有測試的內容搬到真實
//	磁碟上重跑。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- 檔案系統探針 ---------------------------------------------------------

// Linux magic numbers（<linux/magic.h>）。
const (
	magicTmpfs = 0x01021994
	magicExt   = 0xEF53 // ext2/ext3/ext4 共用
	magicFuse  = 0x65735546
)

// fsTypeOf 回傳 path 所在檔案系統的名稱。
//
// ★ 失敗態不得與任何一種「已知檔案系統」共用輸出：statfs 失敗回傳的字串
// 以 "error:" 開頭，呼叫端一律當作「量不到」，不得當作「不是 tmpfs 所以
// 沒問題」。
func fsTypeOf(path string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "error: " + err.Error()
	}
	switch int64(st.Type) {
	case magicTmpfs:
		return "tmpfs"
	case magicExt:
		return "ext2/ext3/ext4"
	case magicFuse:
		return "fuse"
	}
	return fmt.Sprintf("unknown(0x%x)", st.Type)
}

// ext4Dir 回傳一個位於真實磁碟上的空目錄，並「當場驗證」它真的在 ext4 上。
//
// ★ 落點刻意避開兩個地方：
//   - $XDG_RUNTIME_DIR —— tmpfs，正是本檔要避開的東西
//   - repo 自身 —— rclone FUSE 網路掛載，fsync 語意是 rclone 的不是本包的
//   - ~/.local/share/teamdating/data —— live 服務的資料目錄，不得碰
func ext4Dir(t *testing.T) string {
	t.Helper()
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("HOME 未設定，無法定位真實磁碟目錄")
	}
	// 與 live 資料目錄 ~/.local/share/teamdating/data 同層但不同名。
	base := filepath.Join(home, ".local", "share", "teamdating-durability-test")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	if got := fsTypeOf(base); got != "ext2/ext3/ext4" {
		t.Skipf("%s 的檔案系統是 %s，不是 ext4 —— 本測試需要真實磁碟才有意義", base, got)
	}
	dir, err := os.MkdirTemp(base, "d-")
	if err != nil {
		t.Fatalf("mkdtemp in %s: %v", base, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// tmpfsDir 回傳一個位於 tmpfs 上的空目錄，作為對照組使用。
// 取不到就回空字串 —— 呼叫端必須明確處理「沒有對照組」，不得當作通過。
func tmpfsDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" || fsTypeOf(base) != "tmpfs" {
		return ""
	}
	dir, err := os.MkdirTemp(base, "d-")
	if err != nil {
		return ""
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// --- 1. 探針本身要有判別力 -------------------------------------------------

// TestDurabilityProbeHasDiscriminatingPower 驗證「這個目錄在 ext4 上」這個
// 判準本身分得出 ext4 與 tmpfs。
//
// ★ 沒有這一條，後面所有測試都可能在 tmpfs 上全綠而什麼都沒證明。探針對
// 兩種檔案系統回同一個答案時，它的判別力是 0 —— 而那個 0 不會有任何錯誤
// 訊息，它只會讓所有測試看起來很健康。
func TestDurabilityProbeHasDiscriminatingPower(t *testing.T) {
	realDir := ext4Dir(t)
	realFS := fsTypeOf(realDir)
	if realFS != "ext2/ext3/ext4" {
		t.Fatalf("測試目錄 %s 回報 %s，不是 ext4", realDir, realFS)
	}
	t.Logf("受測目錄  %s → %s", realDir, realFS)

	ramDir := tmpfsDir(t)
	if ramDir == "" {
		t.Fatal("拿不到 tmpfs 對照目錄 —— 沒有對照組就無法證明探針有判別力，" +
			"這是「量不到」而不是「通過」")
	}
	ramFS := fsTypeOf(ramDir)
	t.Logf("對照目錄  %s → %s", ramDir, ramFS)

	if ramFS != "tmpfs" {
		t.Fatalf("對照目錄回報 %s，預期 tmpfs", ramFS)
	}
	if realFS == ramFS {
		t.Fatalf("探針對 ext4 與 tmpfs 回同一個答案 (%s) —— 判別力為零，"+
			"本檔所有測試的綠燈都不能採信", realFS)
	}

	// 順帶記錄既有 helper 的落點，這正是本檔要補的缺口。
	t.Logf("對照：store_test.go 的 tempDir() 使用 $XDG_RUNTIME_DIR → %s（fsync 為 no-op）",
		fsTypeOf(os.Getenv("XDG_RUNTIME_DIR")))
}

// --- 2. append() 真的發出 fsync 系統呼叫 -----------------------------------

// TestAppendIssuesRealFsyncSyscall 用 strace 數 append() 實際發出的 fsync
// 系統呼叫次數。
//
// ★ 為什麼需要這一條：使用者空間分不出「fsync 有被呼叫」與「fsync 沒被
// 呼叫」—— 因為 SIGKILL 不會清 page cache，兩種寫法的檔案內容完全相同。
// 唯一能直接觀察到呼叫本身的位置是系統呼叫層。
//
// ★ 控制組是「差分」而不是「絕對數」，這是本測試唯一有判別力的地方：
//
//	go test 行程本身有背景 fsync（測試框架寫檔、覆蓋率、runtime），所以
//	「數到 N 次 fsync」本身不能證明任何事 —— 那個 N 可能全部來自雜訊。
//	改為跑兩趟：一趟做 few 次變更，一趟做 many 次，兩者只差在變更次數。
//	背景雜訊在兩趟中是常數，相減後抵消；差分就是 append() 自己發出的。
//
//	這使得將生產碼的 s.log.Sync() 註解掉時，差分會搌到 0，測試轉紅。
//	子行程跑的是真的 Store，不是模擬寫檔 —— 否則這條測的是「這個目錄的
//	fsync 有效」而不是「append() 有呼叫 fsync」，對生產碼零判別力。
func TestAppendIssuesRealFsyncSyscall(t *testing.T) {
	if os.Getenv("TD_FSYNC_CHILD") == "1" {
		fsyncChild(t)
		return
	}
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skipf("strace 不在 PATH（%v）—— 這是「量不到」，不是「通過」", err)
	}

	// 兩趟只差在變更次數。每次 SetVote 走一次 commit → 一次 append → 一次 fsync。
	const few, many = 2, 12
	nFew := straceCountFsync(t, few)
	nMany := straceCountFsync(t, many)
	delta := nMany - nFew

	t.Logf("fsync 系統呼叫：%d 次變更→%d 次  %d 次變更→%d 次  差分=%d（預期≈%d）",
		few, nFew, many, nMany, delta, many-few)

	// 差分必須跟著變更次數走。寬一點（只要至少一半）以容 snapshot 等
	// 其他路徑的差異，但 0 絕對不可接受 —— 0 就是「append 沒在 fsync」。
	if delta < (many-few)/2 {
		t.Errorf("多做 %d 次變更只多出 %d 次 fsync —— append() 未為每筆變更發出 fsync，"+
			"寫入的位元組只在 page cache 裡", many-few, delta)
	}
	if delta == 0 {
		t.Fatal("差分為 0 —— 變更次數完全不影響 fsync 次數，持久性契約不成立")
	}
}

// fsyncChild 是自我重入的子行程：開一個真的 Store 並做 N 次變更。
//
// ★ 跑真 Store 而非模擬寫檔，是這條測試能不能綁到生產碼的分水嶺。
func fsyncChild(t *testing.T) {
	dir := os.Getenv("TD_FSYNC_DIR")
	n := 0
	fmt.Sscanf(os.Getenv("TD_FSYNC_N"), "%d", &n)

	s, err := New(dir)
	if err != nil {
		t.Fatalf("child New: %v", err)
	}
	defer s.Close()
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatalf("child CreateUser: %v", err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatalf("child CreateProposal: %v", err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-12-28", "2027-01-03", ""); err != nil {
		t.Fatalf("child CreateOption: %v", err)
	}
	// 這 n 次是兩趟之間唯一的差別。輪替權重以確保每次都真的落一筆事件。
	for i := 0; i < n; i++ {
		w := WeightYes
		if i%2 == 1 {
			w = WeightNo
		}
		if err := s.SetVote("P1", "O1", "U1", w); err != nil {
			t.Fatalf("child SetVote %d: %v", i, err)
		}
	}
}

// straceCountFsync 在 strace 下跑一趟子行程（做 n 次變更），回傳觀察到的
// fsync/fdatasync 次數。
func straceCountFsync(t *testing.T, n int) int {
	t.Helper()
	dir := ext4Dir(t)
	out := filepath.Join(dir, "strace.txt")

	cmd := exec.Command("strace", "-f", "-e", "trace=fsync,fdatasync", "-o", out,
		os.Args[0], "-test.run", "^TestAppendIssuesRealFsyncSyscall$", "-test.v")
	cmd.Env = append(os.Environ(),
		"TD_FSYNC_CHILD=1",
		"TD_FSYNC_DIR="+dir,
		fmt.Sprintf("TD_FSYNC_N=%d", n),
	)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("strace 子行程失敗: %v\n%s", err, b)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("讀 strace 輸出: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		// 只數真正成功返回的呼叫，排除 "unfinished"/"resumed" 的重複行。
		if (strings.Contains(line, "fsync(") || strings.Contains(line, "fdatasync(")) &&
			!strings.Contains(line, "unfinished") && !strings.Contains(line, "resumed") {
			count++
		}
	}
	return count
}

// --- 3. 那個 fsync 在 ext4 上有真實成本 -------------------------------------

// TestFsyncHasRealCostOnExt4 量測 fsync 在 ext4 上不是 no-op。
//
// ★ 這一條補的是 strace 補不到的東西：strace 證明「呼叫發出去了」，但呼叫
// 發出去而底層是 no-op（tmpfs 正是如此）仍然什麼都沒保證。時間是唯一能
// 在使用者空間分辨「真的落盤」與「呼叫了但沒事發生」的訊號。
//
// ★ 判準刻意「不用絕對門檻」（例如 >1ms）—— 絕對數字在不同硬體上會壞掉，
// 而壞掉的方向是「變成永遠通過」。改為自我校準：同一台機器上，ext4 的
// 每次 fsync 成本必須顯著高於 tmpfs 的。tmpfs 那組就是控制組。
func TestFsyncHasRealCostOnExt4(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳過計時測試")
	}
	realDir := ext4Dir(t)
	ramDir := tmpfsDir(t)
	if ramDir == "" {
		t.Skip("拿不到 tmpfs 對照目錄 —— 沒有控制組就無法自我校準，這是「量不到」不是「通過」")
	}

	const n = 100
	extWith, extWithout := timeAppends(t, realDir, n)
	ramWith, ramWithout := timeAppends(t, ramDir, n)

	t.Logf("ext4   有 fsync=%v  無 fsync=%v", extWith.Round(time.Microsecond), extWithout.Round(time.Microsecond))
	t.Logf("tmpfs  有 fsync=%v  無 fsync=%v  ← 控制組", ramWith.Round(time.Microsecond), ramWithout.Round(time.Microsecond))

	// ext4 上 fsync 必須比不 fsync 明顯慢。倍數取 5x —— 實測約 1000x，
	// 留大量餘裕給慢速 CI，但仍遠高於 tmpfs 的約 1.3x。
	if extWith < extWithout*5 {
		t.Errorf("ext4 上 fsync 幾乎沒有成本（%v vs %v）—— 這個目錄的 fsync 可能是 no-op，"+
			"持久性測試在這裡跑不具意義", extWith, extWithout)
	}
	// 控制組：tmpfs 上不該看到同樣的落差。若 tmpfs 也慢 5x，代表這個測法
	// 量到的是別的東西（排程雜訊、page cache 行為），不是 fsync。
	if ramWith >= ramWithout*5 {
		t.Errorf("tmpfs 上 fsync 也慢了 5 倍以上（%v vs %v）—— 本測法量到的不是 fsync，"+
			"ext4 那組的綠燈不可採信", ramWith, ramWithout)
	}
}

// timeAppends 對 dir 下的檔案做 n 次「寫一行」，分別量有 fsync 與無 fsync 的耗時。
func timeAppends(t *testing.T, dir string, n int) (with, without time.Duration) {
	t.Helper()
	run := func(name string, doSync bool) time.Duration {
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		defer f.Close()
		line := []byte(`{"t":"2026-08-05T00:00:00Z","type":"vote.set","d":{"pid":"P1"}}` + "\n")
		// 暖身：先跑幾次讓配置/中繼資料成本不落在計時窗內。
		for i := 0; i < 5; i++ {
			f.Write(line)
			if doSync {
				f.Sync()
			}
		}
		start := time.Now()
		for i := 0; i < n; i++ {
			if _, err := f.Write(line); err != nil {
				t.Fatalf("write: %v", err)
			}
			if doSync {
				if err := f.Sync(); err != nil {
					t.Fatalf("sync: %v", err)
				}
			}
		}
		return time.Since(start)
	}
	return run("with.log", true), run("without.log", false)
}

// --- 4. 順序契約在 ext4 上依然成立 ------------------------------------------

// TestWriteAheadOrderOnRealDisk 把既有的 write-ahead 順序控制組（TV-12）搬到
// 真實磁碟上重跑。
//
// ★ 這一條「不」證明斷電後資料還在 —— 見檔頭。它證明的是：既有測試在
// tmpfs 上得到的順序結論，換到會真的落盤的檔案系統上依然成立。兩者的差別
// 在於這裡的 append() 每次都付出真實的 fsync 成本（§3 已量），所以「順序
// 正確」這件事是在真實 I/O 時序下成立的，不是在記憶體檔案系統的理想時序下。
//
// ★ 控制組與既有測試同源：同一個中斷點，正確順序 vs 反轉順序，兩臂必須
// 得到不同結果。相同就代表切點沒切在 commit 窗內，測試沒有判別力。
func TestWriteAheadOrderOnRealDisk(t *testing.T) {
	good := crashDuringVoteAt(t, ext4Dir(t), false)
	broken := crashDuringVoteAt(t, ext4Dir(t), true)

	t.Logf("ext4 上：正確順序保住投票=%v  反轉順序保住投票=%v", good, broken)

	if !good {
		t.Error("ext4 上正確順序遺失了資料 —— write-ahead 契約在真實磁碟上不成立")
	}
	if broken {
		t.Error("ext4 上反轉順序仍保住資料 —— 本測試沒有切在 append 與記憶體寫入之間，無判別力")
	}
	if good == broken {
		t.Fatalf("兩種順序表現相同（正確=%v 反轉=%v）—— 這個持久性測試什麼都沒量到", good, broken)
	}
}

// crashDuringVoteAt 與 store_test.go 的 crashDuringVote 同構，差別只在
// 目錄由呼叫端指定（既有版本寫死使用 tmpfs 上的 tempDir）。
func crashDuringVoteAt(t *testing.T, dir string, brokenOrder bool) (voteSurvived bool) {
	t.Helper()

	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.SetUnsafeOrderForControlGroup(brokenOrder)
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-12-28", "2027-01-03", ""); err != nil {
		t.Fatal(err)
	}

	const crashMarker = "crash: interrupted inside the commit window (ext4)"
	crashed := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if msg, ok := r.(string); ok && msg == crashMarker {
					crashed = true
					return
				}
				panic(r) // 不是我們丟的 —— 不得吞掉真正的 bug
			}
		}()
		s.SetCrashHookForControlGroup(func() { panic(crashMarker) })
		_ = s.SetVote("P1", "O1", "U1", WeightYes)
	}()
	if !crashed {
		t.Fatal("崩潰鉤子沒有觸發 —— 中斷沒有落在 commit 窗內，本輪無判別力")
	}

	// 模擬重啟：完全不理會崩潰後的記憶體狀態，從磁碟重建。
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("replay 後重開: %v", err)
	}
	defer s2.Close()
	return s2.HasVote("P1", "O1", "U1")
}
