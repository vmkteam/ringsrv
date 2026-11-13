package files

import (
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/files/filestest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prices is a workbook as an export of a price list looks: a hidden service
// sheet first, the table with shared and inline strings, a number, a date by
// a built-in and by a custom format, a boolean, a formula's cached value, a
// skipped row and a skipped column; and a second sheet.
func prices(t *testing.T) []byte {
	t.Helper()
	return filestest.XLSX(t, filestest.Workbook{
		Shared: []string{"code", "name", "price", "until", "Аспирин", "Бинт\tстерильный\nширокий"},
		// Style 0 is General, 1 a built-in date, 2 a custom date, 3 money.
		Styles:  []int{0, 14, 164, 165},
		Formats: map[int]string{164: `dd/mm/yyyy\ hh:mm`, 165: `[$₽-419]#,##0.00;[Red]-#,##0.00`},
		Sheets: []filestest.Sheet{
			{Name: "service", Hidden: true, Rows: `<row r="1"><c r="A1" t="inlineStr"><is><t>hidden</t></is></c></row>`},
			{Name: "Прайс", Rows: `` +
				`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c><c r="D1" t="s"><v>3</v></c></row>` +
				`<row r="2"><c r="A2"><v>809000001</v></c><c r="B2" t="s"><v>4</v></c><c r="C2" s="3"><v>125.5</v></c><c r="D2" s="1"><v>46388</v></c></row>` +
				`<row r="4"><c r="A4"><v>809000002</v></c><c r="B4" t="s"><v>5</v></c><c r="C4" s="3"><f>C2*2</f><v>251</v></c><c r="D4" s="2"><v>46388.5</v></c><c r="F4" t="b"><v>1</v></c></row>` +
				`<row r="5"><c r="B5" t="inlineStr"><is><r><t>аспирин </t></r><r><t>детский</t></r><rPh><t>x</t></rPh></is></c><c r="C5" t="e"><v>#N/A</v></c></row>`},
			{Name: "Итого", Rows: `<row r="1"><c r="A1" t="str"><v>total</v></c><c r="B1"><v>2</v></c></row>`},
		},
	})
}

func TestSheet_Rows(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000101", "prices.xlsx", "xlsx", mimeXLSX, "hosted", prices(t), nil)

	res, err := fetch(t, f, "/files/download?file=F0000000101", 32<<10)
	require.NoError(t, err)
	require.NotNil(t, res.Text)
	txt := res.Text
	assert.Equal(t, "Прайс", txt.Sheet, "the first sheet that is not hidden")
	assert.Equal(t, []string{"service", "Прайс", "Итого"}, txt.Sheets)
	assert.Equal(t, []string{
		"1: code\tname\tprice\tuntil",
		"2: 809000001\tАспирин\t125.5\t2027-01-01",
		"3: ",
		"4: 809000002\tБинт стерильный широкий\t251\t2027-01-01 12:00:00\t\tTRUE",
		"5: \tаспирин детский\t#N/A",
	}, txt.Lines, "line N is row N; a cell keeps its column; tabs and newlines inside a cell become spaces")
	assert.Equal(t, 5, txt.TotalLines)
	assert.Empty(t, txt.Header, "read from the top, the header is line 1")
}

func TestSheet_GrepKeepsHeader(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000102", "prices.xlsx", "xlsx", mimeXLSX, "hosted", prices(t), nil)

	res, err := fetch(t, f, "/files/download?file=F0000000102&grep=%D0%B0%D1%81%D0%BF%D0%B8%D1%80%D0%B8%D0%BD", 32<<10)
	require.NoError(t, err)
	assert.Equal(t, "code\tname\tprice\tuntil", res.Text.Header)
	assert.Equal(t, []string{"2: 809000001\tАспирин\t125.5\t2027-01-01", "5: \tаспирин детский\t#N/A"}, res.Text.Lines)
}

func TestSheet_Pick(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000103", "prices.xlsx", "xlsx", "application/octet-stream", "hosted", prices(t), nil)

	for _, sheet := range []string{"%D0%98%D1%82%D0%BE%D0%B3%D0%BE", "%D0%B8%D1%82%D0%BE%D0%B3%D0%BE", "3"} {
		res, err := fetch(t, f, "/files/download?file=F0000000103&sheet="+sheet, 32<<10)
		require.NoError(t, err, sheet)
		assert.Equal(t, "Итого", res.Text.Sheet, "by name, by name without case, by number")
		assert.Equal(t, []string{"1: total\t2"}, res.Text.Lines)
	}
	res, err := fetch(t, f, "/files/download?file=F0000000103&sheet=service", 32<<10)
	require.NoError(t, err)
	assert.Equal(t, []string{"1: hidden"}, res.Text.Lines, "a hidden sheet is read when asked for")

	_, err = fetch(t, f, "/files/download?file=F0000000103&sheet=4", 32<<10)
	require.ErrorIs(t, err, ErrBadParams)
	assert.Contains(t, err.Error(), "service, Прайс, Итого", "the refusal names the sheets there are")
}

func TestSheet_Refused(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000104", "broken.xlsx", "xlsx", mimeXLSX, "hosted", []byte("PK not really"), nil)
	f.Add("F0000000105", "report.csv", "csv", "text/csv", "hosted", []byte("a;b\n"), nil)
	f.Add("F0000000106", "far.xlsx", "xlsx", mimeXLSX, "hosted", filestest.XLSX(t, filestest.Workbook{
		Sheets: []filestest.Sheet{{Name: "a", Rows: `<row r="1048577"><c r="A1048577"><v>1</v></c></row>`}},
	}), nil)

	_, err := fetch(t, f, "/files/download?file=F0000000104", 32<<10)
	assert.Equal(t, CodeUnsupported, refusalCode(err))

	_, err = fetch(t, f, "/files/download?file=F0000000105&sheet=1", 32<<10)
	require.ErrorIs(t, err, ErrBadParams, "a sheet of a file that is not a workbook")

	_, err = fetch(t, f, "/files/download?file=F0000000106", 32<<10)
	assert.Equal(t, CodeUnsupported, refusalCode(err), "a row past the last row of a sheet")
}

func TestIsDateFormat(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]bool{
		"dd/mm/yyyy":                      true,
		`dd/mm/yyyy\ hh:mm`:               true,
		"[$-419]d mmmm yyyy":              true,
		"h:mm AM/PM":                      true,
		"[h]:mm":                          true,
		"[mm]":                            true,
		"General":                         false,
		"0.00":                            false,
		"#,##0":                           false,
		"[$₽-419]#,##0.00;[Red]-#,##0.00": false,
		`"days: "0`:                       false,
		"0.00E+00":                        false,
		`0\d`:                             false,
		"@":                               false,
		"_(* #,##0_)":                     false,
	} {
		assert.Equal(t, want, isDateFormat(code), code)
	}
	assert.True(t, builtinDate(14))
	assert.True(t, builtinDate(22))
	assert.False(t, builtinDate(4))
	assert.False(t, builtinDate(49))
}

func TestSerialDate(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "2027-01-01", serialDate("46388", false))
	assert.Equal(t, "2027-01-01 12:00:00", serialDate("46388.5", false))
	assert.Equal(t, "06:00:00", serialDate("0.25", false))
	assert.Equal(t, "1900-01-01", serialDate("2", false), "the 1900 system counts from 1899-12-30")
	assert.Equal(t, "1904-01-02", serialDate("1", true))
	assert.Equal(t, "abc", serialDate("abc", false))
	assert.Equal(t, "-1", serialDate("-1", false))
}

func TestColumn(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]int{"A1": 0, "B7": 1, "Z3": 25, "AA1": 26, "XFD1048576": 16383} {
		got, ok := column(ref)
		assert.True(t, ok, ref)
		assert.Equal(t, want, got, ref)
	}
	_, ok := column("")
	assert.False(t, ok)
	_, ok = column("12")
	assert.False(t, ok)
}
