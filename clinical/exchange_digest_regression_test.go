package clinical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 本文件为交换包摘要补充“有实际核对价值”的回归测试。
//
// 既有的 TestPackageDigestOrderIndependent 只能证明摘要与入参顺序无关、
// 取包摘要与创建时一致；这两点无法证明摘要真的覆盖了包内信息。接收方凭
// 摘要确认收到的内容，因此这里固定一个同时含诊断与医嘱、字段全部可独立
// 核对的合成样例，逐项验证：患者、接收方、每条记录的就诊、类别、记录标识、
// 版本标识、版本号、非整秒生效时间与完整文字中的任一项变化，摘要都必须
// 变化；一个字符、一个换行、一个首尾空格的差别也不能被吞掉。
//
// 预期摘要不取自产品代码的输出，而是三路独立核对同一个锚点 digestGolden：
//
//  1. goldenCanonicalBytes 按当前摘要格式（exchange.go 中
//     canonicalPackage/canonicalRecord 的字段、顺序与编码规则）逐字节手写，
//     连 JSON 转义都不使用 encoding/json，避免“用同一套序列化库证明自己”；
//  2. independentDigest 是另一份独立实现（匿名结构体加 insertion sort）；
//  3. 产品侧 packageDigest。
//
// digestGolden 的来历与复算方式：对 goldenCanonicalBytes 输出的字节运行
// 系统 sha256sum（小写十六进制）即可得到同一值，不依赖本产品运行结果：
//
//	go test ./clinical/ -run TestScratchEmitGolden -v
//	sha256sum /tmp/clinical-golden-canonical.json
//
// 格式说明（被锚点字节固定下来）：顶层字段顺序 patient_id、receiver_id、
// records；records 按 record_id 升序；每条记录字段顺序 encounter_id、
// category、record_id、version_id、version、effective_at、content；
// 时间统一为 UTC 的 RFC3339Nano；JSON 为紧凑编码，小于号、大于号、与号按
// encoding/json 默认 HTML 转义为  \u003c / \u003e / \u0026 ，引号、反斜杠与
// 控制符按 JSON 转义，其余 UTF-8（含中文、空格、换行）原样输出。
//
// 为避免源码里直接出现尖括号，正文中的小于号/大于号在 Go 字面量里用
// 转义形式写出，运行时仍是同样的原始字符。

// 原始特殊字符（运行时与普通 ASCII 字符完全等价）。
var (
	ltByte  = string(rune(0x3c)) // 小于号
	gtByte  = string(rune(0x3e)) // 大于号
	ampByte = string(rune(0x26)) // 与号
)

// ---- 固定合成样例：标识互不相同，且区分记录标识/版本标识/版本号 ----

const (
	digestFixedPatientID   ID     = "pat_digest_fixed"
	digestFixedReceiverID  string = "rcv_digest_fixed"
	digestFixedEncounterID ID     = "enc_digest_fixed"
	digestFixedRecordDiag  ID     = "rec_digest_diag"
	digestFixedRecordOrder ID     = "rec_digest_order"
	digestFixedVerDiag     ID     = "ver_diag_v2"
	digestFixedVerOrder    ID     = "ver_ord_v1"
)

// 非整秒生效时刻：带纳秒分量，摘要必须保留该精度。
var digestFixedEffectiveAt = time.Date(2026, 3, 15, 7, 30, 15, 267854321, time.UTC)

// 诊断正文：中文、引号、反斜杠、小于号、与号、行间换行与尾随制表符，
// 换行之前还刻意保留一个尾随空格。
const digestDiagContent = "2型糖尿病（诊断）血糖空腹 \u003c 7.0 mmol/L 且 A1C\u003c6.5%；" +
	"备注：引号\"中文引号\"、反斜杠路径 C:\\病历\\血糖 与 \u0026 均按原文 \n" +
	"第二行，尾随制表符\t"

// 医嘱正文：同样混入特殊字符与换行，且与诊断内容不同，证明诊断与医嘱
// 遵守同一条完整性规则。
const digestOrderContent = "复查 空腹血糖 与 糖化血红蛋白（HbA1c）；" +
	"剂量关系 a\u003cb 且 b\u0026\u0026c；不得删去首尾空格 \n第二行（医嘱）"

// digestFixedPkg 用手写标识与内容拼出固定包：诊断与医嘱各一条，诊断版本号
// 2（版本标识 ver_diag_v2）、医嘱版本号 1，三类标识互不相同。记录故意
// “医嘱在前、诊断在后”，验证摘要与排列顺序无关。
func digestFixedPkg() Package {
	return Package{
		PatientID:  digestFixedPatientID,
		ReceiverID: digestFixedReceiverID,
		Records: []PackagedRecord{
			{
				EncounterID: digestFixedEncounterID,
				Category:    Order,
				RecordID:    digestFixedRecordOrder,
				VersionID:   digestFixedVerOrder,
				Version:     1,
				EffectiveAt: digestFixedEffectiveAt,
				Content:     digestOrderContent,
			},
			{
				EncounterID: digestFixedEncounterID,
				Category:    Diagnosis,
				RecordID:    digestFixedRecordDiag,
				VersionID:   digestFixedVerDiag,
				Version:     2,
				EffectiveAt: digestFixedEffectiveAt,
				Content:     digestDiagContent,
			},
		},
	}
}

// digestGolden 是固定样例规范字节的 SHA-256（小写十六进制）：由外部
// sha256sum 对 goldenCanonicalBytes 的输出复算，而不是抄产品返回值。
const digestGolden = "e930f0717ae267d0072e86b17aade151373f2b0faf6075f8e76cc85e5fed13c8"

// ---- 独立核对端 1：手写规范字节（不使用 encoding/json） ----

var hexLower = "0123456789abcdef"

// appendJSONStringBody 按当前摘要格式逐字节追加 JSON 字符串的“引号内内容”：
// 与 encoding/json 在默认 HTML 转义下对字符串的转义规则一致，但这里是
// 手写实现，用于独立核对产品序列化。
func appendJSONStringBody(b []byte, s string) []byte {
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			// 与 encoding/json 一致：U+2028/U+2029 也转义；其余 UTF-8 原样。
			if r == 0x2028 || r == 0x2029 {
				b = append(b, []byte(`\u202`)...)
				if r == 0x2028 {
					b = append(b, '8')
				} else {
					b = append(b, '9')
				}
			} else {
				b = append(b, s[i:i+size]...)
			}
			i += size
			continue
		}
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		case 0x3c: // 小于号
			b = append(b, []byte(`\u003c`)...)
		case 0x3e: // 大于号
			b = append(b, []byte(`\u003e`)...)
		case 0x26: // 与号
			b = append(b, []byte(`\u0026`)...)
		default:
			if c < 0x20 {
				b = append(b, '\\', 'u', '0', '0', hexLower[c>>4], hexLower[c&0x0f])
			} else {
				b = append(b, c)
			}
		}
		i++
	}
	return b
}

func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	b = appendJSONStringBody(b, s)
	b = append(b, '"')
	return b
}

// goldenCanonicalBytes 完全按格式说明手写固定包的规范字节：排序、字段顺序、
// UTC RFC3339Nano 时间与 JSON 转义都在这里显式完成。
func goldenCanonicalBytes(p Package) []byte {
	recs := append([]PackagedRecord(nil), p.Records...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })

	var b []byte
	b = append(b, `{"patient_id":`...)
	b = appendJSONString(b, p.PatientID)
	b = append(b, `,"receiver_id":`...)
	b = appendJSONString(b, p.ReceiverID)
	b = append(b, `,"records":[`...)
	for i, r := range recs {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"encounter_id":`...)
		b = appendJSONString(b, r.EncounterID)
		b = append(b, `,"category":`...)
		b = appendJSONString(b, r.Category)
		b = append(b, `,"record_id":`...)
		b = appendJSONString(b, r.RecordID)
		b = append(b, `,"version_id":`...)
		b = appendJSONString(b, r.VersionID)
		b = append(b, `,"version":`...)
		b = append(b, strconv.Itoa(r.Version)...)
		b = append(b, `,"effective_at":`...)
		b = appendJSONString(b, r.EffectiveAt.UTC().Format(time.RFC3339Nano))
		b = append(b, `,"content":`...)
		b = appendJSONString(b, r.Content)
		b = append(b, '}')
	}
	b = append(b, `]}`...)
	return b
}

// 转义规则本身先被一组字面预期锁定：输入与输出都在测试里写死，不看产品行为。
func TestGoldenJSONEscaperRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 引号内的 JSON 文本（不含两端引号）
	}{
		{"html chars", "a" + ltByte + "b " + gtByte + "c " + ampByte + "d", `a\u003cb \u003ec \u0026d`},
		{"quote and backslash", "\"" + `\`, `\"\\`},
		{"newline and tab", "\n\t", `\n\t`},
		{"control char", "a\x01b", `a\u0001b`},
		{"cjk and spaces passthrough", "中文 引号 末尾 ", "中文 引号 末尾 "},
		{"raw angle in cjk text", "A1C" + ltByte + "6.5% 与", `A1C\u003c6.5% 与`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(appendJSONStringBody(nil, c.in))
			if got != c.want {
				t.Fatalf("escape %q:\n got: %q\nwant: %q", c.in, got, c.want)
			}
		})
	}
}

// ---- 独立核对端 2：另一份 encoding/json 实现（匿名结构体加 insertion sort） ----

func independentDigest(p Package) (string, string) {
	type rec struct {
		EncounterID ID     `json:"encounter_id"`
		Category    string `json:"category"`
		RecordID    ID     `json:"record_id"`
		VersionID   ID     `json:"version_id"`
		Version     int    `json:"version"`
		EffectiveAt string `json:"effective_at"`
		Content     string `json:"content"`
	}
	cp := struct {
		PatientID  ID     `json:"patient_id"`
		ReceiverID string `json:"receiver_id"`
		Records    []rec  `json:"records"`
	}{PatientID: p.PatientID, ReceiverID: p.ReceiverID}

	recs := append([]PackagedRecord(nil), p.Records...)
	for i := 1; i < len(recs); i++ { // insertion sort，刻意不用 sort.Slice
		for j := i; j > 0 && recs[j-1].RecordID > recs[j].RecordID; j-- {
			recs[j-1], recs[j] = recs[j], recs[j-1]
		}
	}
	for _, r := range recs {
		cp.Records = append(cp.Records, rec{
			EncounterID: r.EncounterID,
			Category:    r.Category,
			RecordID:    r.RecordID,
			VersionID:   r.VersionID,
			Version:     r.Version,
			EffectiveAt: r.EffectiveAt.UTC().Format(time.RFC3339Nano),
			Content:     r.Content,
		})
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), string(raw)
}

// ---- 固定样例：三路实现与外部锚点必须一致；格式保持当前形态 ----

var digestHexShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestPackageDigestFixedGoldenValue(t *testing.T) {
	pkg := digestFixedPkg()

	// 固定样例正文确实携带所有要求的字面字符与空白形态，golden 才有意义。
	for _, literal := range []string{
		"中文引号", `"`, `\`, ltByte, ampByte, "\n", "\t",
		`C:\病历\血糖`, "A1C" + ltByte + "6.5%", "均按原文 \n",
	} {
		if !strings.Contains(digestDiagContent, literal) {
			t.Fatalf("diagnosis sample must contain %q", literal)
		}
	}
	// 医嘱正文同样混入小于号、与号、换行与首尾空格（引号/反斜杠由诊断正文承载）。
	for _, literal := range []string{"a" + ltByte + "b", "b" + ampByte + ampByte + "c", " \n第二行（医嘱）"} {
		if !strings.Contains(digestOrderContent, literal) {
			t.Fatalf("order sample must contain %q", literal)
		}
	}

	goldenBytes := goldenCanonicalBytes(pkg)
	sum := sha256.Sum256(goldenBytes)
	goldenFromHandBytes := hex.EncodeToString(sum[:])
	if goldenFromHandBytes != digestGolden {
		t.Fatalf("sha256(hand-written canonical bytes) = %q, want external golden %q\nbytes: %s",
			goldenFromHandBytes, digestGolden, goldenBytes)
	}

	indDigest, indRaw := independentDigest(pkg)
	if indRaw != string(goldenBytes) {
		t.Fatalf("independent canonical bytes differ from hand-written bytes:\n ind: %s\nhand: %s",
			indRaw, goldenBytes)
	}
	if indDigest != digestGolden {
		t.Fatalf("independent digest = %q, want golden %q", indDigest, digestGolden)
	}
	if got := packageDigest(pkg); got != digestGolden {
		t.Fatalf("packageDigest = %q, want golden %q", got, digestGolden)
	}
	if !digestHexShape.MatchString(digestGolden) {
		t.Fatalf("golden %q is not a lowercase hex 64-char digest", digestGolden)
	}

	// 再显式反转记录顺序：同一 golden，摘要与排列顺序无关。
	reversed := Package{PatientID: pkg.PatientID, ReceiverID: pkg.ReceiverID}
	for i := len(pkg.Records) - 1; i >= 0; i-- {
		reversed.Records = append(reversed.Records, pkg.Records[i])
	}
	if got := packageDigest(reversed); got != digestGolden {
		t.Fatalf("reversed-order digest = %q, want golden %q", got, digestGolden)
	}
	if got := string(goldenCanonicalBytes(reversed)); got != string(goldenBytes) {
		t.Fatal("hand-written canonical bytes depend on record order")
	}
}

// ---- 包内每一项信息变化都必须改变摘要（诊断与医嘱同一规则） ----

func digestBasePkg(category string) Package {
	return Package{
		PatientID:  "pat_a",
		ReceiverID: "rcv_a",
		Records: []PackagedRecord{{
			EncounterID: "enc_a",
			Category:    category,
			RecordID:    "rec_a",
			VersionID:   "ver_a",
			Version:     2,
			EffectiveAt: time.Date(2026, 5, 1, 10, 0, 0, 123456789, time.UTC),
			Content:     "基线内容",
		}},
	}
}

func TestPackageDigestCoversEveryPackageField(t *testing.T) {
	for _, category := range []string{Diagnosis, Order} {
		base := digestBasePkg(category)
		baseDigest := packageDigest(base)

		cases := []struct {
			name   string
			mutate func(*Package)
		}{
			{"patient id", func(p *Package) { p.PatientID = "pat_b" }},
			{"receiver id", func(p *Package) { p.ReceiverID = "rcv_b" }},
			{"encounter id", func(p *Package) { p.Records[0].EncounterID = "enc_b" }},
			{"category", func(p *Package) {
				if category == Diagnosis {
					p.Records[0].Category = Order
				} else {
					p.Records[0].Category = Diagnosis
				}
			}},
			{"record id", func(p *Package) { p.Records[0].RecordID = "rec_b" }},
			{"version id", func(p *Package) { p.Records[0].VersionID = "ver_b" }},
			{"version number", func(p *Package) { p.Records[0].Version = 3 }},
			{"effective time", func(p *Package) {
				p.Records[0].EffectiveAt = p.Records[0].EffectiveAt.Add(time.Nanosecond)
			}},
			{"content", func(p *Package) { p.Records[0].Content = "基线内容。" }},
		}

		for _, c := range cases {
			t.Run(category+"/"+c.name, func(t *testing.T) {
				changed := digestBasePkg(category)
				c.mutate(&changed)
				if got := packageDigest(changed); got == baseDigest {
					t.Fatalf("digest unchanged after %s changed (category %s)", c.name, category)
				}
			})
		}
	}
}

// 记录标识、版本标识与版本号是三个独立字段，不能并入同一槽位。
func TestPackageDigestDistinguishesRecordVersionIdentifiersAndNumber(t *testing.T) {
	base := digestBasePkg(Diagnosis)
	d := packageDigest(base)

	onlyRecordID := digestBasePkg(Diagnosis)
	onlyRecordID.Records[0].RecordID = "rec_b"
	onlyVersionID := digestBasePkg(Diagnosis)
	onlyVersionID.Records[0].VersionID = "ver_b"
	onlyNumber := digestBasePkg(Diagnosis)
	onlyNumber.Records[0].Version = 20

	for name, p := range map[string]Package{
		"record id changed":               onlyRecordID,
		"version id changed":              onlyVersionID,
		"version number changed, id same": onlyNumber,
	} {
		if packageDigest(p) == d {
			t.Fatalf("%s did not change digest", name)
		}
	}
	if packageDigest(onlyVersionID) == packageDigest(onlyNumber) {
		t.Fatal("version id and version number are not distinguished from each other")
	}

	// 记录标识与版本标识互换的两种摆法必须两两不同，且都不同于基线。
	swapA := digestBasePkg(Diagnosis)
	swapA.Records[0].RecordID = "rec_a"
	swapA.Records[0].VersionID = "ver_b"
	swapB := digestBasePkg(Diagnosis)
	swapB.Records[0].RecordID = "rec_b"
	swapB.Records[0].VersionID = "ver_a"
	da, db := packageDigest(swapA), packageDigest(swapB)
	if da == d || db == d || da == db {
		t.Fatalf("record/version identifiers collapse: base=%s swapA=%s swapB=%s", d, da, db)
	}
}

// ---- 文字必须按原文逐字参与摘要：一个字符、换行、首尾空格都不能被吞 ----

func TestPackageDigestContentIsByteExact(t *testing.T) {
	for _, category := range []string{Diagnosis, Order} {
		original := "首行\n次行 A " + ltByte + " " + ampByte + ` \ "`
		base := digestBasePkg(category)
		base.Records[0].Content = original
		d := packageDigest(base)

		mutants := []struct {
			name    string
			content string
		}{
			{"one ascii rune replaced", strings.Replace(original, "A", "B", 1)},
			{"one cjk rune replaced", strings.Replace(original, "首", "末", 1)},
			{"leading space added", " " + original},
			{"trailing space added", original + " "},
			{"trailing newline added", original + "\n"},
			{"newline removed", strings.Replace(original, "\n", "", 1)},
			{"newline replaced by space", strings.Replace(original, "\n", " ", 1)},
			{"backslash removed", strings.Replace(original, `\`, "", 1)},
		}
		for _, m := range mutants {
			mp := digestBasePkg(category)
			mp.Records[0].Content = m.content
			if packageDigest(mp) == d {
				t.Fatalf("category %s: digest treats %q as identical content", category, m.name)
			}
		}
	}
}

// ---- 生效时间：时区表示无关、纳秒级变化可区分、整秒表示稳定 ----

func TestPackageDigestEffectiveAtTimezoneAndPrecision(t *testing.T) {
	locPlus5 := time.FixedZone("UTC+5", 5*60*60)
	locPlus530 := time.FixedZone("UTC+5:30", 5*60*60+30*60)
	locMinus8 := time.FixedZone("UTC-8", -8*60*60)

	// 同一物理时刻的四种时区表示（含日期跨天的负偏移）。
	utcInstant := time.Date(2026, 3, 15, 7, 30, 15, 267854321, time.UTC)
	sameInstant := []time.Time{
		utcInstant,
		time.Date(2026, 3, 15, 12, 30, 15, 267854321, locPlus5),
		time.Date(2026, 3, 15, 13, 0, 15, 267854321, locPlus530),
		time.Date(2026, 3, 14, 23, 30, 15, 267854321, locMinus8),
	}
	d := packageDigest(digestPkgWithTime(utcInstant))
	for i, at := range sameInstant {
		if got := packageDigest(digestPkgWithTime(at)); got != d {
			t.Fatalf("case %d: same instant in zone %s gave different digest:\n %s\n %s",
				i, at.Location(), got, d)
		}
		if !at.Equal(utcInstant) {
			t.Fatalf("test setup: %v is not the same instant as %v", at, utcInstant)
		}
	}

	// 时刻真实变化一纳秒必须区分；截掉纳秒分量也必须区分。
	if got := packageDigest(digestPkgWithTime(utcInstant.Add(time.Nanosecond))); got == d {
		t.Fatal("digest does not distinguish a one-nanosecond change")
	}
	if got := packageDigest(digestPkgWithTime(utcInstant.Truncate(time.Second))); got == d {
		t.Fatal("digest does not distinguish truncating fractional nanoseconds")
	}

	// 整秒时刻：不同时区表示归一化为无小数秒的同一字符串，摘要一致；
	// 且不会与上面带纳秒分量的时刻发生表示碰撞。
	wholeUTC := time.Date(2026, 3, 15, 7, 30, 15, 0, time.UTC)
	wholeOffset := time.Date(2026, 3, 15, 12, 30, 15, 0, locPlus5)
	g1, g2 := packageDigest(digestPkgWithTime(wholeUTC)), packageDigest(digestPkgWithTime(wholeOffset))
	if g1 != g2 {
		t.Fatalf("whole-second instant unstable across timezones:\n %s\n %s", g1, g2)
	}
	if g1 == d {
		t.Fatal("whole-second representation collided with the fractional-second instant")
	}
}

func digestPkgWithTime(at time.Time) Package {
	p := digestBasePkg(Diagnosis)
	p.Records[0].EffectiveAt = at
	return p
}

// ---- 包外信息：请求号、发起的内部使用者、交换/授权标识不影响摘要 ----

// 端到端：不同发起者、请求号、绑定授权、交换标识，只要固化的包内容相同，
// 创建交换与接收方取包得到的内容与摘要就必须完全一致。
func TestExchangeDigestIndependentOfCreatorRequestAuthorizationAndIDs(t *testing.T) {
	f := setupExchange(t)

	x1, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-outside-1")
	if err != nil {
		t.Fatal(err)
	}
	// 另一内部使用者、另一请求号、相反顺序：同一患者/接收方/授权与记录集合。
	x2, err := f.s.CreateExchange(doc2, f.pid, rcv.ID, f.auth.ID, []ID{f.ord, f.diag}, "req-outside-2")
	if err != nil {
		t.Fatal(err)
	}
	// 再换一条同样覆盖该集合的绑定授权与请求号。
	authAlt, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}, {EncounterID: f.e1, Category: Order}},
		f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	x3, err := f.s.CreateExchange(doc2, f.pid, rcv.ID, authAlt.ID, []ID{f.diag, f.ord}, "req-outside-3")
	if err != nil {
		t.Fatal(err)
	}

	if x1.ID == x2.ID || x2.ID == x3.ID {
		t.Fatal("exchanges must have distinct ids")
	}
	if x1.CreatorID == x2.CreatorID {
		t.Fatal("test setup: x1 and x2 should be created by different internal actors")
	}
	if x1.Digest != x2.Digest || x2.Digest != x3.Digest {
		t.Fatalf("package-outside fields changed the digest:\n %s\n %s\n %s", x1.Digest, x2.Digest, x3.Digest)
	}
	if !reflect.DeepEqual(x1.Package, x2.Package) || !reflect.DeepEqual(x2.Package, x3.Package) {
		t.Fatalf("frozen packages differ:\n %+v\n %+v\n %+v", x1.Package, x2.Package, x3.Package)
	}

	for _, x := range []Exchange{x1, x2, x3} {
		del, err := f.s.FetchPackage(rcv, x.ID)
		if err != nil {
			t.Fatalf("fetch %s: %v", x.ID, err)
		}
		if del.Digest != x1.Digest {
			t.Fatalf("delivery digest for %s = %s, want %s", x.ID, del.Digest, x1.Digest)
		}
		if !reflect.DeepEqual(del.Package, x.Package) {
			t.Fatalf("delivery package differs from created package:\n %+v\n %+v", del.Package, x.Package)
		}
	}
}

// ---- 端到端：特殊正文与非整秒生效时间穿过创建/取包/重开仍可独立核对 ----

func TestExchangeDigestEndToEndSpecialContentAndNonIntegerTime(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)}
	s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatal(err)
	}

	p, err := s.RegisterPatient(doc, "摘要核对患者")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	diagDraft, err := s.CreateDraft(doc, p.ID, enc.ID, Diagnosis, digestDiagContent)
	if err != nil {
		t.Fatal(err)
	}
	ordDraft, err := s.CreateDraft(doc, p.ID, enc.ID, Order, digestOrderContent)
	if err != nil {
		t.Fatal(err)
	}

	// 非整秒生效：两条记录在不同的带纳秒分量时刻生效。
	diagAt := time.Date(2026, 6, 1, 8, 30, 15, 267854321, time.UTC)
	ordAt := diagAt.Add(7*time.Hour + 33*time.Nanosecond)
	clk.t = diagAt
	diagV, err := s.ActivateRecord(doc, diagDraft.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = ordAt
	ordV, err := s.ActivateRecord(doc, ordDraft.ID)
	if err != nil {
		t.Fatal(err)
	}

	auth, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: enc.ID, Category: Diagnosis}, {EncounterID: enc.ID, Category: Order}},
		diagAt.Add(-time.Hour), ordAt.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	clk.t = ordAt.Add(time.Minute)

	x, err := s.CreateExchange(doc, p.ID, rcv.ID, auth.ID,
		[]ID{ordDraft.ID, diagDraft.ID}, "req-special-content")
	if err != nil {
		t.Fatal(err)
	}

	// 创建结果固化的正是带特殊字符的原文与非整秒时刻，且记录/版本标识可区分。
	gotByID := map[ID]PackagedRecord{}
	for _, pr := range x.Package.Records {
		gotByID[pr.RecordID] = pr
	}
	gotDiag := gotByID[diagDraft.ID]
	if gotDiag.Content != digestDiagContent || gotDiag.VersionID != diagV.ID || gotDiag.Version != 1 ||
		!gotDiag.EffectiveAt.Equal(diagAt) {
		t.Fatalf("diagnosis frozen incorrectly: %+v", gotDiag)
	}
	gotOrd := gotByID[ordDraft.ID]
	if gotOrd.Content != digestOrderContent || gotOrd.VersionID != ordV.ID || gotOrd.Version != 1 ||
		!gotOrd.EffectiveAt.Equal(ordAt) {
		t.Fatalf("order frozen incorrectly: %+v", gotOrd)
	}
	if gotDiag.VersionID == gotOrd.VersionID || gotDiag.RecordID == diagV.ID {
		t.Fatal("record and version identifiers must be distinct stable ids")
	}

	// 对“实际交付给接收方的包”独立重算摘要：不引用 x.Digest。
	independent, raw := independentDigest(x.Package)
	if independent != x.Digest {
		t.Fatalf("exchange digest %s != independent digest %s\ncanonical: %s", x.Digest, independent, raw)
	}
	if string(goldenCanonicalBytes(x.Package)) != raw {
		t.Fatal("hand-written canonical bytes disagree with the independent oracle on delivered package")
	}

	// 指定接收方取包：同一份内容、同一个摘要、原文未被任何转义改写。
	del, err := s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if del.Digest != x.Digest {
		t.Fatalf("delivery digest %s != created digest %s", del.Digest, x.Digest)
	}
	if !reflect.DeepEqual(del.Package, x.Package) {
		t.Fatalf("delivery package differs from created package:\n %+v\n %+v", del.Package, x.Package)
	}
	for _, pr := range del.Package.Records {
		switch pr.Category {
		case Diagnosis:
			if pr.Content != digestDiagContent {
				t.Fatalf("diagnosis content altered in delivery: %q", pr.Content)
			}
		case Order:
			if pr.Content != digestOrderContent {
				t.Fatalf("order content altered in delivery: %q", pr.Content)
			}
		}
	}

	// 回执核对行为保留：原摘要接受成功；仅加一个尾随空格的篡改摘要必须被拒。
	if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, ""); err != nil {
		t.Fatalf("receipt with matching digest: %v", err)
	}
	tamperedPkg := x.Package
	tamperedPkg.Records = append([]PackagedRecord(nil), x.Package.Records...)
	tamperedPkg.Records[0].Content = tamperedPkg.Records[0].Content + " "
	tamperedSum := sha256.Sum256(goldenCanonicalBytes(tamperedPkg))
	if hex.EncodeToString(tamperedSum[:]) == x.Digest {
		t.Fatal("trailing-space tamper did not change digest")
	}
	if _, err := s.SubmitReceipt(rcv, x.ID, hex.EncodeToString(tamperedSum[:]), ReceiptAccepted, ""); err == nil {
		t.Fatal("receipt accepted a digest computed from altered content")
	}

	// 关闭重开：JSON 落盘往返后特殊字符、纳秒时刻与摘要都必须保持不变。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	redel, err := s2.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if redel.Digest != x.Digest {
		t.Fatalf("digest changed after reopen: %s != %s", redel.Digest, x.Digest)
	}
	if !reflect.DeepEqual(redel.Package, x.Package) {
		t.Fatalf("package changed across reopen:\n %+v\n %+v", redel.Package, x.Package)
	}
	if !redel.CreatedAt.Equal(x.CreatedAt) {
		t.Fatalf("exchange created-at unstable across reopen: %v != %v", redel.CreatedAt, x.CreatedAt)
	}
}
