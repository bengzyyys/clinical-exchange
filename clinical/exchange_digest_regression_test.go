package clinical

// 交换包摘要的回归测试。
//
// 既有测试（见 exchange_test.go）已证明：记录入参顺序不同摘要相同、取包得到的
// 摘要与创建时一致。但那只能证明“两次计算彼此一致”，不能证明摘要确实覆盖了
// 包内的关键信息。本文件补上“完整性”回归：
//
//   - 固定金向量：参与摘要的规范 JSON 文档与其 sha256 以常量形式钉死。该向量
//     由独立的生成程序手工逐字段拼装（不调用本包的 canonicalRecord/packageDigest
//     ），再用 coreutils sha256sum 与 Python 反解析双重核对，因此期望值有明确
//     依据，而不是“产品本次算出什么就写什么”。
//   - 字段敏感性：患者、接收方、每条记录的就诊、类别、记录标识、版本标识、
//     版本号、非整秒生效时间与完整文字，任一项变化摘要都必须变化；一个字符、
//     一个换行、一个首尾空格的差别也不能被吞掉。诊断与医嘱在同一矩阵下逐一
//     验证，遵守同一项完整性规则。
//   - 表达稳定性：同一时刻换时区表示摘要相同；时刻相差一纳秒摘要不同。
//   - 包外信息（请求号、发起的内部使用者、交换标识、绑定授权标识）不参与摘要。
//
// 摘要格式（UTF-8 JSON、固定字段顺序、时间为 UTC RFC3339Nano、64 位十六进制
// 小写 sha256）与创建/取包/回执既有调用方式保持不变，本文件不引入新格式或
// 新的交换操作。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---- 固定金向量 ----
//
// 样例刻意让“记录标识 / 版本标识 / 版本号”三者明显不同（rec_*、ver_*、2 与 7），
// 避免把版本号写进标识或把标识误当字段值的实现蒙混过关；生效时间刻意不是整秒
// （带 9 位小数秒）；正文同时包含中文、引号、反斜杠、小于号、与号和换行，用于
// 证明这些字符参与核对后文字含义仍被原样保留。
const (
	goldenPatientID   = "pat_digest_golden_01"
	goldenReceiverID  = "rcv_digest_golden"
	goldenEncounterID = "enc_digest_golden_01"

	goldenDiagRecordID  = "rec_diag_golden"
	goldenDiagVersionID = "ver_diag_golden_2nd"
	goldenDiagVersionNo = 2

	goldenOrderRecordID  = "rec_ord_golden"
	goldenOrderVersionID = "ver_ord_golden_7th"
	goldenOrderVersionNo = 7
)

// 非整秒生效时刻（UTC）。
var goldenEffective = time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC)

// 诊断与医嘱正文：均含中文、半角双引号、反斜杠、小于号、与号与换行。
const goldenDiagContent = "2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a<b 与 c&d；目录 C:\\病历"
const goldenOrderContent = "医嘱：胰岛素 8IU（餐前）\n注意 \"剂量<10IU 需复核\"；配伍 5%GS&0.9%NS；路径 C:\\泵注"

// wantGoldenCanonicalJSON 是参与摘要的精确规范字节（记录按记录标识排序后：
// diagnosis 在 order 之前）。该文档由独立程序手工拼装写出，并用
// `sha256sum canonical.json` 复核与下列摘要一致。
const wantGoldenCanonicalJSON = `{"patient_id":"pat_digest_golden_01","receiver_id":"rcv_digest_golden","records":[{"encounter_id":"enc_digest_golden_01","category":"diagnosis","record_id":"rec_diag_golden","version_id":"ver_diag_golden_2nd","version":2,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a\u003cb 与 c\u0026d；目录 C:\\病历"},{"encounter_id":"enc_digest_golden_01","category":"order","record_id":"rec_ord_golden","version_id":"ver_ord_golden_7th","version":7,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量\u003c10IU 需复核\"；配伍 5%GS\u00260.9%NS；路径 C:\\泵注"}]}`

// wantGoldenDigest = sha256(wantGoldenCanonicalJSON)，由独立工具与 sha256sum 双重得出。
const wantGoldenDigest = "f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87"

var lowercaseHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// goldenPackage 按金向量常量在内存中独立拼出包结构（不经过产品的创建流程），
// 供字段敏感性矩阵直接对 packageDigest 取样。
func goldenPackage() Package {
	return Package{
		PatientID:  goldenPatientID,
		ReceiverID: goldenReceiverID,
		Records: []PackagedRecord{
			{
				EncounterID: goldenEncounterID,
				Category:    Diagnosis,
				RecordID:    goldenDiagRecordID,
				VersionID:   goldenDiagVersionID,
				Version:     goldenDiagVersionNo,
				EffectiveAt: goldenEffective,
				Content:     goldenDiagContent,
			},
			{
				EncounterID: goldenEncounterID,
				Category:    Order,
				RecordID:    goldenOrderRecordID,
				VersionID:   goldenOrderVersionID,
				Version:     goldenOrderVersionNo,
				EffectiveAt: goldenEffective,
				Content:     goldenOrderContent,
			},
		},
	}
}

func copyPackage(p Package) Package {
	cp := p
	cp.Records = append([]PackagedRecord(nil), p.Records...)
	return cp
}

// withRecord 返回包的副本，并对指定记录标识的那条记录施加改动。
func withRecord(p Package, recordID ID, fn func(*PackagedRecord)) Package {
	cp := copyPackage(p)
	for i := range cp.Records {
		if cp.Records[i].RecordID == recordID {
			fn(&cp.Records[i])
		}
	}
	return cp
}

// ---- 金向量夹具：用公开 API 合法地造出“诊断 + 医嘱”，再把随机标识改写成金向量常量 ----
//
// 产品的患者/就诊/记录/版本标识由 crypto/rand 生成，调用方无法指定；而金向量
// 需要固定标识。因此先用真实业务流程（登记→就诊→草稿→生效→更正→授权）造出
// 已授权、合法的内容，再在测试进程内把快照标识改名为常量（与既有白盒测试
// 直接操作 s.data 的做法一致，见 exchange_test.go 的排队场景）。改名不改变任何
// 业务关系，随后的创建交换/取包/回执仍全部走公开 API 与真实授权检查。

type goldenStore struct {
	s      *Store
	clk    *fakeClock
	dir    string
	rcv    Actor
	authID ID
}

func openGoldenStore(t *testing.T) goldenStore {
	t.Helper()
	dir := t.TempDir()
	clk := &fakeClock{t: goldenEffective.Add(-2 * time.Hour)}
	s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// 患者与就诊。
	clk.t = goldenEffective.Add(-2 * time.Hour)
	p, err := s.RegisterPatient(doc, "摘要金向量合成患者")
	if err != nil {
		t.Fatal(err)
	}
	clk.t = goldenEffective.Add(-90 * time.Minute)
	enc, err := s.AddEncounter(doc, p.ID, goldenEffective.Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 诊断：先生效 v1，再更正为 v2——当前版本号为 2，版本标识与记录标识不同。
	clk.t = goldenEffective.Add(-time.Hour)
	d, err := s.CreateDraft(doc, p.ID, enc.ID, Diagnosis, "诊断初稿")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, d.ID); err != nil {
		t.Fatal(err)
	}
	clk.t = goldenEffective
	dv2, err := s.CorrectRecord(doc, d.ID, 1, goldenDiagContent, "复核补全编码与符号")
	if err != nil {
		t.Fatal(err)
	}

	// 医嘱：生效后连续更正，当前版本号为 7，版本标识同样与记录标识不同；
	// 最终版本在 goldenEffective 时刻定稿，内容含全部特殊字符。
	clk.t = goldenEffective.Add(-30 * time.Minute)
	o, err := s.CreateDraft(doc, p.ID, enc.ID, Order, "医嘱初稿")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, o.ID); err != nil {
		t.Fatal(err)
	}
	for v := 2; v <= 6; v++ {
		clk.t = goldenEffective.Add(time.Duration(v-7) * time.Minute)
		if _, err := s.CorrectRecord(doc, o.ID, v-1, "医嘱中途修订版", "持续修订"); err != nil {
			t.Fatal(err)
		}
	}
	clk.t = goldenEffective
	ov7, err := s.CorrectRecord(doc, o.ID, 6, goldenOrderContent, "定稿")
	if err != nil {
		t.Fatal(err)
	}

	// 已授权：整类覆盖该就诊下的诊断与医嘱，窗口包住 goldenEffective。
	a, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{
			{EncounterID: enc.ID, Category: Diagnosis},
			{EncounterID: enc.ID, Category: Order},
		},
		goldenEffective.Add(-24*time.Hour), goldenEffective.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 把随机标识改写成金向量常量。
	s.mu.Lock()
	snap := s.data

	pat := snap.Patients[p.ID]
	delete(snap.Patients, p.ID)
	pat.ID = goldenPatientID
	snap.Patients[goldenPatientID] = pat

	e := snap.Encounters[enc.ID]
	delete(snap.Encounters, enc.ID)
	e.ID = goldenEncounterID
	e.PatientID = goldenPatientID
	snap.Encounters[goldenEncounterID] = e

	renameRecord := func(oldRID, newRID, currentOldVID, currentNewVID ID) {
		r := snap.Records[oldRID]
		delete(snap.Records, oldRID)
		r.ID = newRID
		r.PatientID = goldenPatientID
		r.EncounterID = goldenEncounterID
		for i, vid := range r.Versions {
			if vid == currentOldVID {
				r.Versions[i] = currentNewVID
			}
		}
		r.CurrentVersionID = currentNewVID
		snap.Records[newRID] = r
		// 先把该记录全部历史版本的归属改到新记录标识。
		for _, ver := range snap.Versions {
			if ver.RecordID == oldRID {
				ver.RecordID = newRID
			}
		}
		// 再把当前版本改名为金向量版本标识（版本号保持不变：2 与 7）。
		cur := snap.Versions[currentOldVID]
		delete(snap.Versions, currentOldVID)
		cur.ID = currentNewVID
		cur.RecordID = newRID
		snap.Versions[currentNewVID] = cur
	}
	renameRecord(d.ID, goldenDiagRecordID, dv2.ID, goldenDiagVersionID)
	renameRecord(o.ID, goldenOrderRecordID, ov7.ID, goldenOrderVersionID)

	auth := snap.Authorizations[a.ID]
	auth.PatientID = goldenPatientID
	auth.ReceiverID = goldenReceiverID
	for i := range auth.Scopes {
		auth.Scopes[i].EncounterID = goldenEncounterID
	}

	// 审计事件不参与摘要，但把其中的患者/记录引用一并改写，保持快照内部一致。
	for _, ev := range snap.AuditEvents {
		if ev.PatientID == p.ID {
			ev.PatientID = goldenPatientID
		}
		if ev.ObjectType == "record" {
			switch ev.ObjectID {
			case d.ID:
				ev.ObjectID = goldenDiagRecordID
			case o.ID:
				ev.ObjectID = goldenOrderRecordID
			}
		}
	}
	s.mu.Unlock()

	return goldenStore{
		s: s, clk: clk, dir: dir,
		rcv:    ReceiverActor(goldenReceiverID),
		authID: a.ID,
	}
}

// ---- 1. 金向量：创建交换与接收方取包对应同一份内容、同一个摘要，且摘要可独立核对 ----

func TestExchangeDigestGoldenVector(t *testing.T) {
	g := openGoldenStore(t)

	// 金向量自身必须自洽：常量摘要 == 常量规范文档的 sha256。
	sum := sha256.Sum256([]byte(wantGoldenCanonicalJSON))
	if got := hex.EncodeToString(sum[:]); got != wantGoldenDigest {
		t.Fatalf("golden vector inconsistent: sha256(canonical)=%s, want %s", got, wantGoldenDigest)
	}
	if !lowercaseHex64.MatchString(wantGoldenDigest) {
		t.Fatalf("golden digest format changed: %q", wantGoldenDigest)
	}

	// 规范文档必须能独立反解析回与原始正文逐字一致的内容：小于号、与号、
	// 引号、反斜杠、换行、中文都只是换了一种转义表达，文字含义不变。
	var cp canonicalPackage
	if err := json.Unmarshal([]byte(wantGoldenCanonicalJSON), &cp); err != nil {
		t.Fatalf("canonical json must parse: %v", err)
	}
	if cp.PatientID != goldenPatientID || cp.ReceiverID != goldenReceiverID || len(cp.Records) != 2 {
		t.Fatalf("canonical header wrong: %+v", cp)
	}
	wantRec := map[ID]canonicalRecord{}
	for _, cr := range cp.Records {
		wantRec[cr.RecordID] = cr
	}
	dcr := wantRec[goldenDiagRecordID]
	if dcr.VersionID != goldenDiagVersionID || dcr.Version != goldenDiagVersionNo ||
		dcr.EncounterID != goldenEncounterID || dcr.Category != Diagnosis ||
		dcr.EffectiveAt != "2026-03-14T09:07:05.123456789Z" || dcr.Content != goldenDiagContent {
		t.Fatalf("canonical diagnosis record wrong: %+v", dcr)
	}
	ocr := wantRec[goldenOrderRecordID]
	if ocr.VersionID != goldenOrderVersionID || ocr.Version != goldenOrderVersionNo ||
		ocr.EncounterID != goldenEncounterID || ocr.Category != Order ||
		ocr.EffectiveAt != "2026-03-14T09:07:05.123456789Z" || ocr.Content != goldenOrderContent {
		t.Fatalf("canonical order record wrong: %+v", ocr)
	}

	// 故意以“医嘱在前、诊断在后”的入参顺序创建：包内仍按记录标识排序，
	// 摘要必须正好等于独立推导的金向量（既测顺序无关，也测端到端正确性）。
	x, err := g.s.CreateExchange(doc, goldenPatientID, g.rcv.ID, g.authID,
		[]ID{goldenOrderRecordID, goldenDiagRecordID}, "req-golden")
	if err != nil {
		t.Fatalf("create exchange: %v", err)
	}
	if x.Digest != wantGoldenDigest {
		t.Fatalf("exchange digest = %s\nwant golden   = %s", x.Digest, wantGoldenDigest)
	}

	// 创建结果的包内容必须逐项等于金向量，特殊字符原样保留（不被省略或替换）。
	if !reflect.DeepEqual(x.Package, goldenPackage()) {
		t.Fatalf("created package differs from golden vector:\n%+v\nwant\n%+v", x.Package, goldenPackage())
	}
	for _, pr := range x.Package.Records {
		want := goldenDiagContent
		if pr.RecordID == goldenOrderRecordID {
			want = goldenOrderContent
		}
		if pr.Content != want {
			t.Fatalf("content not preserved verbatim for %q:\n%q\nwant\n%q", pr.RecordID, pr.Content, want)
		}
	}

	// 指定接收方取包：同一份内容、同一个摘要；交付携带当前状态。
	del, err := g.s.FetchPackage(g.rcv, x.ID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if del.Digest != wantGoldenDigest {
		t.Fatalf("delivery digest = %s, want %s", del.Digest, wantGoldenDigest)
	}
	if !lowercaseHex64.MatchString(del.Digest) {
		t.Fatalf("digest format must stay 64 lowercase hex chars, got %q", del.Digest)
	}
	if !reflect.DeepEqual(del.Package, x.Package) {
		t.Fatalf("fetched package differs from created package:\n%+v\n%+v", del.Package, x.Package)
	}
	if del.ExchangeID != x.ID || del.Status != ExchangePending {
		t.Fatalf("delivery envelope wrong: %+v", del)
	}
}

// ---- 2. 字段敏感性矩阵：包内任一关键信息变化都必须改变摘要 ----

func TestPackageDigestChangesWhenAnyFieldChanges(t *testing.T) {
	base := goldenPackage()
	d0 := packageDigest(base)
	if d0 != wantGoldenDigest {
		t.Fatalf("independently assembled package digest = %s, want golden %s", d0, wantGoldenDigest)
	}

	cases := []struct {
		name   string
		mutate func(Package) Package
	}{
		{"patient id", func(p Package) Package { p.PatientID += "x"; return p }},
		{"receiver id", func(p Package) Package { p.ReceiverID += "x"; return p }},

		{"encounter on both records", func(p Package) Package {
			for i := range p.Records {
				p.Records[i].EncounterID = goldenEncounterID + "_other"
			}
			return p
		}},
		{"encounter on diagnosis only", func(p Package) Package {
			return withRecord(p, goldenDiagRecordID, func(r *PackagedRecord) { r.EncounterID += "x" })
		}},
		{"encounter on order only", func(p Package) Package {
			return withRecord(p, goldenOrderRecordID, func(r *PackagedRecord) { r.EncounterID += "x" })
		}},

		{"category diagnosis -> order", func(p Package) Package {
			return withRecord(p, goldenDiagRecordID, func(r *PackagedRecord) { r.Category = Order })
		}},
		{"category order -> diagnosis", func(p Package) Package {
			return withRecord(p, goldenOrderRecordID, func(r *PackagedRecord) { r.Category = Diagnosis })
		}},

		{"record id of diagnosis", func(p Package) Package {
			return withRecord(p, goldenDiagRecordID, func(r *PackagedRecord) { r.RecordID += "x" })
		}},
		{"record id of order", func(p Package) Package {
			return withRecord(p, goldenOrderRecordID, func(r *PackagedRecord) { r.RecordID += "x" })
		}},

		{"version id of diagnosis", func(p Package) Package {
			return withRecord(p, goldenDiagRecordID, func(r *PackagedRecord) { r.VersionID += "x" })
		}},
		{"version id of order", func(p Package) Package {
			return withRecord(p, goldenOrderRecordID, func(r *PackagedRecord) { r.VersionID += "x" })
		}},

		// 版本号与版本标识是两个不同字段：只改版本号（标识不动）也必须被发现。
		{"version number of diagnosis", func(p Package) Package {
			return withRecord(p, goldenDiagRecordID, func(r *PackagedRecord) { r.Version = 3 })
		}},
		{"version number of order", func(p Package) Package {
			return withRecord(p, goldenOrderRecordID, func(r *PackagedRecord) { r.Version = 8 })
		}},

		// 生效时间精确到纳秒：一纳秒之差、丢掉小数秒都必须被发现。
		{"effective time +1ns on diagnosis", func(p Package) Package {
			return withRecord(p, goldenDiagRecordID, func(r *PackagedRecord) { r.EffectiveAt = r.EffectiveAt.Add(time.Nanosecond) })
		}},
		{"effective time +1ns on order", func(p Package) Package {
			return withRecord(p, goldenOrderRecordID, func(r *PackagedRecord) { r.EffectiveAt = r.EffectiveAt.Add(time.Nanosecond) })
		}},
		{"effective time truncated to whole second", func(p Package) Package {
			return withRecord(p, goldenDiagRecordID, func(r *PackagedRecord) {
				r.EffectiveAt = r.EffectiveAt.Add(-time.Duration(r.EffectiveAt.Nanosecond()))
			})
		}},

		// 记录被整条遗漏或多出一条，都不再是同一个包。
		{"diagnosis record omitted", func(p Package) Package {
			p.Records = append([]PackagedRecord(nil), p.Records[1:]...)
			return p
		}},
		{"order record omitted", func(p Package) Package {
			p.Records = append([]PackagedRecord(nil), p.Records[:1]...)
			return p
		}},
		{"extra record appended", func(p Package) Package {
			cp := copyPackage(p)
			extra := cp.Records[1]
			extra.RecordID = goldenOrderRecordID + "_extra"
			extra.VersionID = goldenOrderVersionID + "_extra"
			cp.Records = append(cp.Records, extra)
			return cp
		}},
	}

	// 完整文字的改动矩阵：同一组改动分别施加在诊断与医嘱上——两类记录
	// 遵守同一项完整性规则。
	contentMutations := []struct {
		name string
		fn   func(string) string
	}{
		// 只改一个字符：两条正文都以中文字符结尾，替换最后一个字符即单字符变化。
		{"one trailing Chinese character", func(s string) string {
			rs := []rune(s)
			rs[len(rs)-1] = 'X'
			return string(rs)
		}},
		// 只删一个换行。
		{"one newline removed", func(s string) string { return strings.Replace(s, "\n", "", 1) }},
		// 只加一个首尾空格。
		{"one leading space", func(s string) string { return " " + s }},
		{"one trailing space", func(s string) string { return s + " " }},
		// 引号、反斜杠、小于号、与号各改一个：转义相关字符被省略/替换都会暴露。
		{"one quote changed", func(s string) string { return strings.Replace(s, `"`, `'`, 1) }},
		{"one backslash changed", func(s string) string { return strings.Replace(s, `\`, `/`, 1) }},
		{"one less-than sign changed", func(s string) string { return strings.Replace(s, `<`, `《`, 1) }},
		{"one ampersand changed", func(s string) string { return strings.Replace(s, `&`, `+`, 1) }},
		// 整段文字被清空：必须与完整正文不同。
		{"content emptied", func(s string) string { return "" }},
	}
	for _, recordID := range []ID{goldenDiagRecordID, goldenOrderRecordID} {
		for _, cm := range contentMutations {
			recordID, cm := recordID, cm
			cases = append(cases, struct {
				name   string
				mutate func(Package) Package
			}{
				name: "content " + cm.name + " on " + recordID,
				mutate: func(p Package) Package {
					return withRecord(p, recordID, func(r *PackagedRecord) { r.Content = cm.fn(r.Content) })
				},
			})
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mutate(copyPackage(base))
			if reflect.DeepEqual(mutated, base) {
				t.Fatalf("mutator %q did not actually change the package", tc.name)
			}
			if got := packageDigest(mutated); got == d0 {
				t.Fatalf("digest unchanged after %q: %s", tc.name, got)
			}
		})
	}
}

// ---- 2b. 记录排列顺序不同摘要相同（直接钉住摘要函数自身的排序承诺）----
//
// 端到端创建路径在打包前已按记录标识排序，既有 TestPackageDigestOrderIndependent
// 覆盖的是那条路径。这里额外直接以逆序/轮换的记录切片调用 packageDigest：
// 一旦其内部“先按记录标识排序再序列化”的步骤被移除，本测试立即失败，
// 而不会因为唯一调用方恰好有序而漏掉退化。
func TestPackageDigestOrderIndependentOfRecordSliceOrder(t *testing.T) {
	base := goldenPackage()
	want := packageDigest(base)

	reversed := copyPackage(base)
	for i, j := 0, len(reversed.Records)-1; i < j; i, j = i+1, j-1 {
		reversed.Records[i], reversed.Records[j] = reversed.Records[j], reversed.Records[i]
	}
	if got := packageDigest(reversed); got != want {
		t.Fatalf("digest changed when record slice order was reversed:\n%s\n%s", got, want)
	}

	// 三条记录的轮换排列，避免只覆盖两元素逆序这一种排列。
	three := copyPackage(base)
	extra := three.Records[0]
	extra.RecordID = "rec_zzz_golden_extra"
	extra.VersionID = "ver_zzz_golden_extra"
	three.Records = append(three.Records, extra)
	wantThree := packageDigest(three)

	rotated := copyPackage(three)
	rotated.Records[0], rotated.Records[1], rotated.Records[2] =
		rotated.Records[2], rotated.Records[0], rotated.Records[1]
	if got := packageDigest(rotated); got != wantThree {
		t.Fatalf("digest changed when three-record slice order was rotated:\n%s\n%s", got, wantThree)
	}
}

// ---- 3. 时间表示：时区无关，但纳秒级真实变化必须可区分 ----

func TestPackageDigestTimeIsInstantBasedAndNanosecondSensitive(t *testing.T) {
	effUTC := goldenEffective
	east := time.FixedZone("UTC+9", 9*60*60)
	west := time.FixedZone("UTC-5", -5*60*60)
	effEast := time.Date(2026, 3, 14, 18, 7, 5, 123456789, east)
	effWest := time.Date(2026, 3, 14, 4, 7, 5, 123456789, west)
	if !effUTC.Equal(effEast) || !effUTC.Equal(effWest) {
		t.Fatal("test setup: zone variants must denote the same instant")
	}

	pkgAt := func(at time.Time) Package {
		return withRecord(goldenPackage(), goldenDiagRecordID, func(r *PackagedRecord) { r.EffectiveAt = at })
	}

	dUTC := packageDigest(pkgAt(effUTC))
	// 同一时刻用 +9、-5 时区表示：摘要一致。
	if got := packageDigest(pkgAt(effEast)); got != dUTC {
		t.Fatalf("digest changed across UTC/+9 representations:\n%s\n%s", got, dUTC)
	}
	if got := packageDigest(pkgAt(effWest)); got != dUTC {
		t.Fatalf("digest changed across UTC/-5 representations:\n%s\n%s", got, dUTC)
	}

	// 时刻真实相差一纳秒：即便用不同时区表述，也必须与原时刻不同，
	// 且“+1ns 的 +9 表述”应与“+1ns 的 UTC 表述”相同。
	plusNanoUTC := effUTC.Add(time.Nanosecond)
	plusNanoEast := time.Date(2026, 3, 14, 18, 7, 5, 123456790, east)
	if !plusNanoUTC.Equal(plusNanoEast) {
		t.Fatal("test setup: +1ns variants must denote the same instant")
	}
	if packageDigest(pkgAt(plusNanoUTC)) == dUTC {
		t.Fatal("digest failed to distinguish instants one nanosecond apart (UTC)")
	}
	if got := packageDigest(pkgAt(plusNanoEast)); got != packageDigest(pkgAt(plusNanoUTC)) {
		t.Fatal("same instant in different zones must give the same digest")
	}

	// 小数秒带尾随零、整秒时刻：跨时区同样稳定（RFC3339Nano 表达不应不稳定）。
	fracUTC := time.Date(2026, 3, 14, 9, 7, 5, 123456000, time.UTC)
	fracEast := time.Date(2026, 3, 14, 18, 7, 5, 123456000, east)
	if packageDigest(pkgAt(fracUTC)) != packageDigest(pkgAt(fracEast)) {
		t.Fatal("trailing-zero fractional second must be stable across zones")
	}
	secUTC := time.Date(2026, 3, 14, 9, 7, 5, 0, time.UTC)
	secWest := time.Date(2026, 3, 14, 4, 7, 5, 0, west)
	if packageDigest(pkgAt(secUTC)) != packageDigest(pkgAt(secWest)) {
		t.Fatal("whole-second instant must be stable across zones")
	}
	if packageDigest(pkgAt(fracUTC)) == packageDigest(pkgAt(secUTC)) {
		t.Fatal("digest must distinguish a fractional-second instant from the whole second")
	}
}

// ---- 4. 包外信息不参与摘要：请求号、发起使用者、交换标识、绑定授权标识 ----

func TestExchangeDigestExcludesEnvelopeMetadata(t *testing.T) {
	g := openGoldenStore(t)
	records := []ID{goldenOrderRecordID, goldenDiagRecordID}

	// 同一包内容：不同内部使用者、不同请求号 → 不同交换，但同一摘要。
	xA, err := g.s.CreateExchange(doc, goldenPatientID, g.rcv.ID, g.authID, records, "req-envelope-a")
	if err != nil {
		t.Fatal(err)
	}
	xB, err := g.s.CreateExchange(doc2, goldenPatientID, g.rcv.ID, g.authID, records, "req-envelope-b")
	if err != nil {
		t.Fatal(err)
	}
	if xA.ID == xB.ID || xA.CreatorID == xB.CreatorID || xA.RequestID == xB.RequestID {
		t.Fatalf("exchanges must differ in envelope metadata:\n%+v\n%+v", xA, xB)
	}
	if xA.Digest != wantGoldenDigest || xA.Digest != xB.Digest {
		t.Fatalf("digest must not depend on creator/request/exchange id: %s vs %s", xA.Digest, xB.Digest)
	}
	if !reflect.DeepEqual(xA.Package, xB.Package) {
		t.Fatal("same inputs must freeze the same package content")
	}

	// 另一条同样合法覆盖该内容的授权：绑定授权标识不同，包内容不变、摘要不变。
	auth2, err := g.s.Grant(doc, goldenPatientID, g.rcv.ID,
		[]Scope{
			{EncounterID: goldenEncounterID, Category: Diagnosis},
			{EncounterID: goldenEncounterID, Category: Order},
		},
		goldenEffective.Add(-24*time.Hour), goldenEffective.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	xC, err := g.s.CreateExchange(doc, goldenPatientID, g.rcv.ID, auth2.ID, records, "req-envelope-c")
	if err != nil {
		t.Fatal(err)
	}
	if xC.AuthorizationID == xA.AuthorizationID || xC.ID == xA.ID {
		t.Fatal("test setup: expected a distinct second authorization and exchange")
	}
	if xC.Digest != wantGoldenDigest {
		t.Fatalf("digest must not depend on authorization id: %s", xC.Digest)
	}

	// 指定接收方分别取包：交换标识不同，拿到的内容与摘要仍是同一份。
	delA, err := g.s.FetchPackage(g.rcv, xA.ID)
	if err != nil {
		t.Fatal(err)
	}
	delB, err := g.s.FetchPackage(g.rcv, xB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if delA.ExchangeID == delB.ExchangeID {
		t.Fatal("delivery exchange ids must differ")
	}
	if delA.Digest != delB.Digest || delA.Digest != wantGoldenDigest {
		t.Fatalf("deliveries must share the golden digest: %s vs %s", delA.Digest, delB.Digest)
	}
	if !reflect.DeepEqual(delA.Package, delB.Package) {
		t.Fatal("deliveries of identical content must carry identical packages")
	}
}

// ---- 5. 关闭重开后摘要保持不变（持久化往返不得丢失纳秒或改写文字） ----

func TestExchangeDigestStableAcrossReopen(t *testing.T) {
	g := openGoldenStore(t)
	x, err := g.s.CreateExchange(doc, goldenPatientID, g.rcv.ID, g.authID,
		[]ID{goldenDiagRecordID, goldenOrderRecordID}, "req-reopen")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(g.dir, WithClock(func() time.Time { return goldenEffective }))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.GetExchange(doc, goldenPatientID, x.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Digest != wantGoldenDigest {
		t.Fatalf("digest after reopen = %s, want %s", got.Digest, wantGoldenDigest)
	}
	if !reflect.DeepEqual(got.Package, goldenPackage()) {
		t.Fatalf("package after reopen differs from golden vector:\n%+v", got.Package)
	}

	// 取包路径重算授权（窗口仍有效），交付的摘要与内容同样不变。
	del, err := s2.FetchPackage(ReceiverActor(goldenReceiverID), x.ID)
	if err != nil {
		t.Fatalf("fetch after reopen: %v", err)
	}
	if del.Digest != wantGoldenDigest || !reflect.DeepEqual(del.Package, got.Package) {
		t.Fatalf("delivery after reopen wrong: digest=%s", del.Digest)
	}
}

// ---- 6. 回执以金向量摘要为准：吻合可登记，篡改一个字符即拒绝 ----

func TestReceiptVerifiesGoldenDigest(t *testing.T) {
	g := openGoldenStore(t)

	x, err := g.s.CreateExchange(doc, goldenPatientID, g.rcv.ID, g.authID,
		[]ID{goldenDiagRecordID, goldenOrderRecordID}, "req-receipt-good")
	if err != nil {
		t.Fatal(err)
	}
	conf, err := g.s.SubmitReceipt(g.rcv, x.ID, wantGoldenDigest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("receipt with golden digest: %v", err)
	}
	if conf.Status != ExchangeAccepted || conf.Outcome != ReceiptAccepted {
		t.Fatalf("bad confirmation: %+v", conf)
	}

	// 另一份包：摘要被悄悄改动一个十六进制字符即视为内容对不上，拒绝登记且
	// 状态不变——接收方正是凭这个值发现文字/字段被改的。
	y, err := g.s.CreateExchange(doc, goldenPatientID, g.rcv.ID, g.authID,
		[]ID{goldenDiagRecordID}, "req-receipt-tampered")
	if err != nil {
		t.Fatal(err)
	}
	tampered := flipLastHexChar(wantGoldenDigest)
	if _, err := g.s.SubmitReceipt(g.rcv, y.ID, tampered, ReceiptAccepted, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("tampered digest err = %v, want ErrConflict", err)
	}
	view, err := g.s.GetExchange(doc, goldenPatientID, y.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != ExchangePending || view.Receipt != nil {
		t.Fatalf("exchange must stay pending after digest mismatch: %+v", view)
	}
}

func flipLastHexChar(d string) string {
	c := d[len(d)-1]
	if c == '0' {
		c = '1'
	} else {
		c = '0'
	}
	return d[:len(d)-1] + string(c)
}
