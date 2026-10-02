package trivia

import (
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

// Сверка стран наблюдений GBIF с ареалом MDD.
//
// GBIF пишет страну кодом ISO 3166-1 alpha-2, MDD — английским именем в
// своём написании («Russia», «Iran», «Cote d'Ivoire»), и с названиями ISO
// они расходятся почти у пятой части стран. Поэтому таблица выписана
// вручную, а не выводится из названий: её полноту проверяет тест по списку
// всех имён стран настоящей базы MDD (testdata/mdd_countries.txt, 241 имя).
//
// Код может соответствовать нескольким именам MDD: MDD выделяет острова,
// которые для ISO — часть страны (Канары — ES, Азоры и Мадейра — PT,
// Галапагосы — EC, Андаманские острова — IN, острова Принс-Эдуард — ZA,
// Вознесение — SH, Саба и Синт-Эстатиус — BQ). Наблюдение с кодом ES для
// эндемика Канар — в ареале: GBIF точнее страны не скажет. Первое имя —
// основное.
//
// Гонконг, Макао, Аландские острова и Шпицберген MDD отдельно не выделяет —
// их наблюдения сверяются с объемлющей страной. Для кодов из
// countryNoMDD имени в MDD нет вовсе (Андорра, Гибралтар, Джерси…): вид там
// может жить, но MDD записал бы его под соседней страной, поэтому такое
// наблюдение — RangeUnknown, а не «вне ареала».
var countryMDD = map[string][]string{
	"AE": {"United Arab Emirates"},
	"AF": {"Afghanistan"},
	"AG": {"Antigua and Barbuda"},
	"AI": {"Anguilla"},
	"AL": {"Albania"},
	"AM": {"Armenia"},
	"AO": {"Angola"},
	"AQ": {"Antarctica"},
	"AR": {"Argentina"},
	"AS": {"American Samoa"},
	"AT": {"Austria"},
	"AU": {"Australia"},
	"AW": {"Aruba"},
	"AX": {"Finland"}, // Åland Islands: MDD не выделяет
	"AZ": {"Azerbaijan"},
	"BA": {"Bosnia and Herzegovina"},
	"BB": {"Barbados"},
	"BD": {"Bangladesh"},
	"BE": {"Belgium"},
	"BF": {"Burkina Faso"},
	"BG": {"Bulgaria"},
	"BH": {"Bahrain"},
	"BI": {"Burundi"},
	"BJ": {"Benin"},
	"BL": {"Saint Barthélemy"},
	"BM": {"Bermuda"},
	"BN": {"Brunei"},
	"BO": {"Bolivia"},
	"BQ": {"Bonaire", "Saba", "Sint Eustatius"},
	"BR": {"Brazil"},
	"BS": {"Bahamas"},
	"BT": {"Bhutan"},
	"BV": {"Bouvet Island"},
	"BW": {"Botswana"},
	"BY": {"Belarus"},
	"BZ": {"Belize"},
	"CA": {"Canada"},
	"CC": {"Cocos Islands"},
	"CD": {"Democratic Republic of the Congo"},
	"CF": {"Central African Republic"},
	"CG": {"Republic of the Congo"},
	"CH": {"Switzerland"},
	"CI": {"Cote d'Ivoire"},
	"CK": {"Cook Islands"},
	"CL": {"Chile"},
	"CM": {"Cameroon"},
	"CN": {"China"},
	"CO": {"Colombia"},
	"CR": {"Costa Rica"},
	"CU": {"Cuba"},
	"CV": {"Cape Verde"},
	"CW": {"Curaçao"},
	"CX": {"Christmas Island"},
	"CY": {"Cyprus"},
	"CZ": {"Czech Republic"},
	"DE": {"Germany"},
	"DJ": {"Djibouti"},
	"DK": {"Denmark"},
	"DM": {"Dominica"},
	"DO": {"Dominican Republic"},
	"DZ": {"Algeria"},
	"EC": {"Ecuador", "Galápagos Islands"},
	"EE": {"Estonia"},
	"EG": {"Egypt"},
	"ER": {"Eritrea"},
	"ES": {"Spain", "Canary Islands"},
	"ET": {"Ethiopia"},
	"FI": {"Finland"},
	"FJ": {"Fiji"},
	"FK": {"Falkland Islands"},
	"FM": {"Micronesia"},
	"FO": {"Faroe"},
	"FR": {"France"},
	"GA": {"Gabon"},
	"GB": {"United Kingdom"},
	"GD": {"Grenada"},
	"GE": {"Georgia"},
	"GF": {"French Guiana"},
	"GH": {"Ghana"},
	"GL": {"Greenland"},
	"GM": {"Gambia"},
	"GN": {"Guinea"},
	"GP": {"Guadeloupe"},
	"GQ": {"Equatorial Guinea"},
	"GR": {"Greece"},
	"GS": {"South Georgia and the South Sandwich Islands"},
	"GT": {"Guatemala"},
	"GU": {"Guam"},
	"GW": {"Guinea-Bissau"},
	"GY": {"Guyana"},
	"HK": {"China"}, // Hong Kong: MDD не выделяет
	"HN": {"Honduras"},
	"HR": {"Croatia"},
	"HT": {"Haiti"},
	"HU": {"Hungary"},
	"ID": {"Indonesia"},
	"IE": {"Ireland"},
	"IL": {"Israel"},
	"IN": {"India", "Andaman and Nicobar Islands"},
	"IQ": {"Iraq"},
	"IR": {"Iran"},
	"IS": {"Iceland"},
	"IT": {"Italy"},
	"JM": {"Jamaica"},
	"JO": {"Jordan"},
	"JP": {"Japan"},
	"KE": {"Kenya"},
	"KG": {"Kyrgyzstan"},
	"KH": {"Cambodia"},
	"KI": {"Kiribati"},
	"KM": {"Comoros"},
	"KN": {"Saint Kitts and Nevis"},
	"KP": {"North Korea"},
	"KR": {"South Korea"},
	"KW": {"Kuwait"},
	"KY": {"Cayman Islands"},
	"KZ": {"Kazakhstan"},
	"LA": {"Laos"},
	"LB": {"Lebanon"},
	"LC": {"Saint Lucia"},
	"LI": {"Liechtenstein"},
	"LK": {"Sri Lanka"},
	"LR": {"Liberia"},
	"LS": {"Lesotho"},
	"LT": {"Lithuania"},
	"LU": {"Luxembourg"},
	"LV": {"Latvia"},
	"LY": {"Libya"},
	"MA": {"Morocco"},
	"MD": {"Moldova"},
	"ME": {"Montenegro"},
	"MG": {"Madagascar"},
	"MH": {"Marshall Islands"},
	"MK": {"North Macedonia"},
	"ML": {"Mali"},
	"MM": {"Myanmar"},
	"MN": {"Mongolia"},
	"MO": {"China"}, // Macao: MDD не выделяет
	"MP": {"Northern Marianas"},
	"MQ": {"Martinique"},
	"MR": {"Mauritania"},
	"MS": {"Montserrat"},
	"MT": {"Malta"},
	"MU": {"Mauritius"},
	"MV": {"Maldives"},
	"MW": {"Malawi"},
	"MX": {"Mexico"},
	"MY": {"Malaysia"},
	"MZ": {"Mozambique"},
	"NA": {"Namibia"},
	"NC": {"New Caledonia"},
	"NE": {"Niger"},
	"NF": {"Norfolk Island"},
	"NG": {"Nigeria"},
	"NI": {"Nicaragua"},
	"NL": {"Netherlands"},
	"NO": {"Norway"},
	"NP": {"Nepal"},
	"NR": {"Nauru"},
	"NU": {"Niue"},
	"NZ": {"New Zealand"},
	"OM": {"Oman"},
	"PA": {"Panama"},
	"PE": {"Peru"},
	"PF": {"French Polynesia"},
	"PG": {"Papua New Guinea"},
	"PH": {"Philippines"},
	"PK": {"Pakistan"},
	"PL": {"Poland"},
	"PN": {"Pitcairn"},
	"PR": {"Puerto Rico"},
	"PS": {"Palestine"},
	"PT": {"Portugal", "Azores", "Madeira"},
	"PW": {"Palau"},
	"PY": {"Paraguay"},
	"QA": {"Qatar"},
	"RE": {"Réunion"},
	"RO": {"Romania"},
	"RS": {"Serbia"},
	"RU": {"Russia"},
	"RW": {"Rwanda"},
	"SA": {"Saudi Arabia"},
	"SB": {"Solomon Islands"},
	"SC": {"Seychelles"},
	"SD": {"Sudan"},
	"SE": {"Sweden"},
	"SG": {"Singapore"},
	"SH": {"Saint Helena", "Ascension"},
	"SI": {"Slovenia"},
	"SJ": {"Norway"}, // Svalbard and Jan Mayen: MDD не выделяет
	"SK": {"Slovakia"},
	"SL": {"Sierra Leone"},
	"SN": {"Senegal"},
	"SO": {"Somalia"},
	"SR": {"Suriname"},
	"SS": {"South Sudan"},
	"ST": {"São Tomé and Príncipe"},
	"SV": {"El Salvador"},
	"SX": {"Sint Maarten"},
	"SY": {"Syria"},
	"SZ": {"Eswatini"},
	"TC": {"Turks and Caicos Islands"},
	"TD": {"Chad"},
	"TF": {"French Southern and Antarctic Lands"},
	"TG": {"Togo"},
	"TH": {"Thailand"},
	"TJ": {"Tajikistan"},
	"TK": {"Tokelau"},
	"TL": {"East Timor"},
	"TM": {"Turkmenistan"},
	"TN": {"Tunisia"},
	"TO": {"Tonga"},
	"TR": {"Turkey"},
	"TT": {"Trinidad and Tobago"},
	"TV": {"Tuvalu"},
	"TW": {"Taiwan"},
	"TZ": {"Tanzania"},
	"UA": {"Ukraine"},
	"UG": {"Uganda"},
	"US": {"United States"},
	"UY": {"Uruguay"},
	"UZ": {"Uzbekistan"},
	"VC": {"Saint Vincent and the Grenadines"},
	"VE": {"Venezuela"},
	"VG": {"British Virgin Islands"},
	"VI": {"United States Virgin Islands"},
	"VN": {"Vietnam"},
	"VU": {"Vanuatu"},
	"WF": {"Wallis and Futuna"},
	"WS": {"Samoa"},
	"XK": {"Kosovo"},
	"YE": {"Yemen"},
	"YT": {"Mayotte"},
	"ZA": {"South Africa", "Prince Edward Islands"},
	"ZM": {"Zambia"},
	"ZW": {"Zimbabwe"},
}

// countryNoMDD — коды GBIF без имени в MDD: название ISO нужно только для
// текста материала.
var countryNoMDD = map[string]string{
	"AD": "Andorra",
	"EH": "Western Sahara",
	"GG": "Guernsey",
	"GI": "Gibraltar",
	"HM": "Heard Island and McDonald Islands",
	"IM": "Isle of Man",
	"IO": "British Indian Ocean Territory",
	"JE": "Jersey",
	"MC": "Monaco",
	"MF": "Saint Martin (French part)",
	"PM": "Saint Pierre and Miquelon",
	"SM": "San Marino",
	"UM": "United States Minor Outlying Islands",
	"VA": "Holy See",
	// Служебные коды GBIF: страна не указана и открытое море.
	"ZZ": "страна не указана",
	"XZ": "открытое море",
}

// countryClassify — имя страны в написании MDD и её место в ареале вида.
// Из нескольких имён кода выбирается то, что есть в ареале: для эндемика
// Канар наблюдение ES — «Canary Islands», а не «Spain». Имена сравниваются
// без учёта регистра: хранилище MDD само приводит страны к нижнему регистру
// в индексе, и на написание в Species полагаться не стоит.
func countryClassify(code string, sp mdd.Species) (name, rng string) {
	code = strings.ToUpper(strings.TrimSpace(code))
	names := countryMDD[code]
	if len(names) == 0 {
		return "", RangeUnknown
	}
	if n, ok := countryFind(names, sp.Countries); ok {
		return n, RangeIn
	}
	if n, ok := countryFind(names, sp.CountriesUncertain); ok {
		return n, RangeUncertain
	}
	return names[0], RangeOut
}

func countryFind(names, list []string) (string, bool) {
	for _, n := range names {
		for _, c := range list {
			if strings.EqualFold(strings.TrimSpace(c), n) {
				return n, true
			}
		}
	}
	return "", false
}

// countryLabel — страна для текста материала: «RU Russia», для кодов без
// имени MDD — название ISO, для неизвестного кода — сам код.
func countryLabel(c CountryCount) string {
	switch {
	case c.Name != "":
		return c.Code + " " + c.Name
	case countryNoMDD[c.Code] != "":
		return c.Code + " " + countryNoMDD[c.Code]
	}
	return c.Code
}
