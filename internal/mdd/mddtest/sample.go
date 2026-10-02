// Package mddtest — тестовые данные и общий набор проверок для хранилищ
// справочного слоя MDD.
//
// Sample — одиннадцать настоящих видов из MDD v2.5 (значения полей взяты из
// MDD_v2.5_6904species.csv как есть) и пять строк Diff_v2.4-v2.5.csv.
// Conformance — проверки контракта mdd.Store, которые обязана проходить
// любая реализация.
package mddtest

import (
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

// ID видов набора — чтобы тесты не повторяли числа.
const (
	Platypus    = 1000001 // Ornithorhynchus anatinus, phylosort 1
	Thylacine   = 1000220 // Thylacinus cynocephalus, вымерший (EX)
	KrugerBat   = 1007038 // Afronycteris rautenbachi, описан в v2.5; phylosort 23 при большом id
	GiantPanda  = 1005930 // Ailuropoda melanoleuca, VU
	RedFox      = 1005977 // Vulpes vulpes
	DomesticCat = 1005985 // Felis catus, domestic
	Lynx        = 1006007 // Lynx lynx, есть страны под вопросом
	Manul       = 1006010 // Otocolobus manul
	Lion        = 1006020 // Panthera leo, VU
	SnowLeopard = 1006024 // Panthera uncia, VU
	BlackRhino  = 1006113 // Diceros bicornis, CR
)

// SampleOrder — ID набора в порядке phylosort, затем id: в этом порядке
// Search отдаёт виды без фильтров.
var SampleOrder = []int{Platypus, Thylacine, KrugerBat, GiantPanda, RedFox,
	DomesticCat, Lynx, Manul, Lion, SnowLeopard, BlackRhino}

// LoadedAt — время загрузки в Sample.Release: с точностью до секунды, в UTC,
// чтобы переживало любое хранилище.
var LoadedAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// Sample — новый экземпляр набора: его можно менять, другим тестам это не
// помешает.
func Sample() *mdd.Dataset {
	species := []mdd.Species{
		{
			ID: Platypus, Phylosort: 1,
			SciName: "Ornithorhynchus anatinus", CommonName: "Platypus",
			OtherCommonNames: []string{"Duck-billed Platypus"},
			Order:            "Monotremata", Family: "Ornithorhynchidae",
			Genus: "Ornithorhynchus", Epithet: "anatinus",
			Authority: "(G. K. Shaw, 1799)", Year: 1799, IUCN: "NT",
			Countries:    []string{"Australia"},
			Continents:   []string{"Oceania (Continent)"},
			Realms:       []string{"Australasia"},
			TypeLocality: `"In Australasia." Restricted to "New Holland (= Sydney), New South Wales, Australia" or the "coastal region of New South Wales" by numerous authors, although this type locality appears to have been erroneously restricted.`,
			DistributionNotes: "E Australia from NE Queensland through W New South Wales and Victoria, including King I and Tasmania. " +
				"Distribution is primarily continuous within some catchments but dicontinuous or poorly known in others. " +
				"Extinct in the Adelaide Hills and Mount Lofty Ranges in South Australia and may be extinct in various catchments " +
				"throughout the rest of their distribution (Hawke et al. 2019). Introduced to E Kangaroo I.",
			TaxonomyNotes: "Originally described in the genus _Platypus_ Shaw, 1799, but this is preoccupied by a genus of beetle " +
				"(_Platypus_ Herbst, 1793), leaving _Ornithorhynchus_ Blumenbach, 1800 as the next available generic name. " +
				"No subspecies have been recognized (Pasitschniak-Arts and Marinelli 1998), although 3-5 divergent clades have been " +
				"identified across the species distribution using microsatellites and mitochondrial DNA (Furlan et al. 2010; " +
				"Gongora et al. 2012; Kolomyjec et al. 2013) that could represent distinct subspecies; the most divergent of these " +
				"lineages is found on Tasmania and King Island, while the genetic structure on mainland Australia is associated with " +
				"river systems (Bino et al. 2019). Size variation is clinal in association to environmental temperature (larger " +
				"animals in colder regions; Furlan et al. 2012), but no other morphological variation has been studied across the " +
				"species distribution. Monotypic.",
		},
		{
			ID: Thylacine, Phylosort: 6,
			SciName: "Thylacinus cynocephalus", CommonName: "Thylacine",
			OtherCommonNames: []string{"Hyena Opossum", "Marsupial Wolf", "Tasmanian Tiger", "Tasmanian Wolf", "Zebra Wolf"},
			Order:            "Dasyuromorphia", Family: "Thylacinidae",
			Genus: "Thylacinus", Epithet: "cynocephalus",
			Authority: "(G. P. R. Harris, 1808)", Year: 1808, IUCN: "EX", Extinct: true,
			Countries:    []string{"Australia", "Indonesia", "Papua New Guinea"},
			Continents:   []string{"Oceania (Continent)"},
			Realms:       []string{"Australasia"},
			TypeLocality: "Tasmania, Australia.",
		},
		{
			ID: KrugerBat, Phylosort: 23,
			SciName: "Afronycteris rautenbachi", CommonName: "Kruger Serotine",
			OtherCommonNames: []string{"Kruger Tail-gland Bat"},
			Order:            "Chiroptera", Family: "Vespertilionidae", Subfamily: "Vespertilioninae",
			Genus: "Afronycteris", Epithet: "rautenbachi",
			Authority: "Kearney, De Vries, & Markotter, 2026", Year: 2026, IUCN: "NE",
			Countries:     []string{"Kenya", "Mozambique", "South Africa"},
			Continents:    []string{"Africa"},
			Realms:        []string{"Afrotropic"},
			TypeLocality:  `"South Africa: Limpopo province, Kruger National Park, Makuleka Contract Park, in a Lala palm grove on the edge of a dry pan, -22.34657 S 31.11595 E (Figure 1). Mist-netted on 3 February 2010."`,
			TaxonomyNotes: "recently described",
		},
		{
			ID: GiantPanda, Phylosort: 25,
			SciName: "Ailuropoda melanoleuca", CommonName: "Giant Panda",
			OtherCommonNames: []string{"Da Xiong Mao"},
			Order:            "Carnivora", Family: "Ursidae", Subfamily: "Ailuropodinae",
			Genus: "Ailuropoda", Epithet: "melanoleuca",
			Authority: "(A. David, 1869)", Year: 1869, IUCN: "VU",
			Countries:    []string{"China"},
			Continents:   []string{"Asia"},
			Realms:       []string{"Palearctic"},
			TypeLocality: "Sichuan Province, China.",
		},
		{
			ID: RedFox, Phylosort: 25,
			SciName: "Vulpes vulpes", CommonName: "Red Fox",
			OtherCommonNames: []string{"Silver Fox", "Cross Fox"},
			Order:            "Carnivora", Family: "Canidae", Subfamily: "Caninae",
			Genus: "Vulpes", Epithet: "vulpes",
			Authority: "(Linnaeus, 1758)", Year: 1758, IUCN: "LC",
			Countries: []string{"Afghanistan", "Albania", "Algeria", "Armenia", "Austria", "Azerbaijan", "Bangladesh",
				"Belarus", "Belgium", "Bhutan", "Bosnia and Herzegovina", "Bulgaria", "Canada", "China", "Croatia",
				"Czech Republic", "Denmark", "Egypt", "Estonia", "Finland", "France", "Georgia", "Germany", "Greece",
				"Greenland", "Hungary", "India", "Iran", "Iraq", "Ireland", "Israel", "Italy", "Japan", "Jordan",
				"Kazakhstan", "Kosovo", "Kuwait", "Kyrgyzstan", "Latvia", "Lebanon", "Libya", "Liechtenstein",
				"Lithuania", "Luxembourg", "Moldova", "Mongolia", "Montenegro", "Morocco", "Myanmar", "Nepal",
				"Netherlands", "North Korea", "North Macedonia", "Norway", "Oman", "Pakistan", "Palestine", "Poland",
				"Portugal", "Qatar", "Romania", "Russia", "Saudi Arabia", "Serbia", "Slovakia", "Slovenia",
				"South Korea", "Spain", "Sweden", "Switzerland", "Syria", "Tajikistan", "Tunisia", "Turkey",
				"Turkmenistan", "Ukraine", "United Arab Emirates", "United Kingdom", "United States", "Uzbekistan", "Yemen"},
			Continents:   []string{"Africa", "Asia", "Europe", "North America"},
			Realms:       []string{"Nearctic", "Palearctic"},
			TypeLocality: "Sweden.",
			TaxonomyNotes: "some publications have recognized two distinct species of red fox, V. vulpes from the Palearctic and " +
				"northwestern Nearctic and V. fulva from the rest of the Nearctic; however, this arrangement leaves Palearctic " +
				"V. vulpes paraphyletic, since some archaic clades of Palearctic red fox (from West Asia and North Africa primarily) " +
				"are sister to the Eurasian + North American clades; the second North American species (fulva) is tentatively " +
				"retained under V. vulpes here pending further studies investigating the position of the archaic Eurasian clades",
		},
		{
			ID: DomesticCat, Phylosort: 25,
			SciName: "Felis catus", CommonName: "Domestic Cat",
			OtherCommonNames: []string{"Cat"},
			Order:            "Carnivora", Family: "Felidae", Subfamily: "Felinae",
			Genus: "Felis", Epithet: "catus",
			Authority: "Linnaeus, 1758", Year: 1758, IUCN: "NE", Domestic: true,
			TypeLocality:  "Sweden.",
			TaxonomyNotes: "domestic form of F. lybica",
		},
		{
			ID: Lynx, Phylosort: 25,
			SciName: "Lynx lynx", CommonName: "Eurasian Lynx",
			Order: "Carnivora", Family: "Felidae", Subfamily: "Felinae",
			Genus: "Lynx", Epithet: "lynx",
			Authority: "(Linnaeus, 1758)", Year: 1758, IUCN: "LC",
			Countries: []string{"Afghanistan", "Armenia", "Austria", "Azerbaijan", "Bosnia and Herzegovina", "Bulgaria",
				"China", "Croatia", "Czech Republic", "Estonia", "Finland", "France", "Georgia", "Germany", "Hungary",
				"India", "Iran", "Iraq", "Kazakhstan", "Kyrgyzstan", "Latvia", "Liechtenstein", "Lithuania", "Mongolia",
				"Nepal", "North Korea", "Norway", "Pakistan", "Poland", "Romania", "Russia", "Slovakia", "Slovenia",
				"Sweden", "Switzerland", "Tajikistan", "Turkey", "Turkmenistan", "Ukraine", "Uzbekistan"},
			CountriesUncertain: []string{"Bhutan", "Greece", "Kosovo", "Moldova", "Montenegro", "Serbia"},
			Continents:         []string{"Asia", "Europe"},
			Realms:             []string{"Palearctic"},
			TypeLocality:       "Wennersborg, S Sweden.",
		},
		{
			ID: Manul, Phylosort: 25,
			SciName: "Otocolobus manul", CommonName: "Pallas's Cat",
			OtherCommonNames: []string{"Manul", "Steppe Cat"},
			Order:            "Carnivora", Family: "Felidae", Subfamily: "Felinae",
			Genus: "Otocolobus", Epithet: "manul",
			Authority: "(Pallas, 1776)", Year: 1776, IUCN: "LC",
			Countries: []string{"Afghanistan", "Armenia", "Bhutan", "China", "India", "Iran", "Kazakhstan", "Kyrgyzstan",
				"Mongolia", "Nepal", "Pakistan", "Russia", "Turkmenistan"},
			CountriesUncertain: []string{"Azerbaijan", "Tajikistan", "Uzbekistan"},
			Continents:         []string{"Asia"},
			Realms:             []string{"Palearctic"},
			TypeLocality:       "S of Lake Baikal, Russia.",
			DistributionNotes:  "recently re-discovered in Armenia",
			TaxonomyNotes:      "moved from Felis to Otocolobus",
		},
		{
			ID: Lion, Phylosort: 25,
			SciName: "Panthera leo", CommonName: "Lion",
			Order: "Carnivora", Family: "Felidae", Subfamily: "Pantherinae",
			Genus: "Panthera", Epithet: "leo",
			Authority: "(Linnaeus, 1758)", Year: 1758, IUCN: "VU",
			Countries: []string{"Angola", "Benin", "Botswana", "Burkina Faso", "Cameroon", "Central African Republic",
				"Chad", "Democratic Republic of the Congo", "Eswatini", "Ethiopia", "Guinea-Bissau", "India", "Kenya",
				"Malawi", "Mozambique", "Namibia", "Niger", "Nigeria", "Senegal", "Somalia", "South Africa",
				"South Sudan", "Sudan", "Tanzania", "Uganda", "Zambia", "Zimbabwe"},
			CountriesUncertain: []string{"Cote d'Ivoire", "Ghana", "Guinea", "Mali", "Rwanda", "Togo"},
			Continents:         []string{"Africa", "Asia"},
			Realms:             []string{"Afrotropic", "Indomalaya"},
			TypeLocality:       "Morocco, North Africa.",
		},
		{
			ID: SnowLeopard, Phylosort: 25,
			SciName: "Panthera uncia", CommonName: "Snow Leopard",
			OtherCommonNames: []string{"Ounce"},
			Order:            "Carnivora", Family: "Felidae", Subfamily: "Pantherinae",
			Genus: "Panthera", Epithet: "uncia",
			Authority: "(Boddaert, 1772)", Year: 1772, IUCN: "VU",
			Countries: []string{"Afghanistan", "Bhutan", "China", "India", "Kazakhstan", "Kyrgyzstan", "Mongolia",
				"Nepal", "Pakistan", "Russia", "Tajikistan", "Uzbekistan"},
			Continents:    []string{"Asia"},
			Realms:        []string{"Palearctic"},
			TypeLocality:  "Kopet-Dagh Mountains, near Iran.",
			TaxonomyNotes: "moved from Uncia to Panthera",
		},
		{
			ID: BlackRhino, Phylosort: 26,
			SciName: "Diceros bicornis", CommonName: "Black Rhinoceros",
			OtherCommonNames: []string{"Hook-lipped Rhinoceros", "Prehensile-lipped Rhinoceros"},
			Order:            "Perissodactyla", Family: "Rhinocerotidae", Subfamily: "Rhinocerotinae",
			Genus: "Diceros", Epithet: "bicornis",
			Authority: "(Linnaeus, 1758)", Year: 1758, IUCN: "CR",
			Countries: []string{"Angola", "Botswana", "Eswatini", "Kenya", "Malawi", "Mozambique", "Namibia", "Rwanda",
				"South Africa", "Tanzania", "Zambia", "Zimbabwe"},
			Continents:   []string{"Africa"},
			Realms:       []string{"Afrotropic"},
			TypeLocality: "South Africa.",
		},
	}

	changes := []mdd.Change{
		{
			NewName: "Afronycteris rautenbachi", Comment: "recently described", Category: "de novo",
			Reference: "Kearney, T.C., Vries, M. de and Markotter, W. 2026-03-09. Description of a new species of African " +
				"pipistrelle-like bat (Chiroptera: Vespertilionidae: Afronycteris). Zootaxa 5768(1):1-28.",
		},
		{
			NewName: "Apnoctomys conicetorum", Comment: "recently described", Category: "de novo",
			Reference: "Teta, P., Ojeda, A.A., Tarquino Carbonell, A.P., Alvarado-Larios, J.R., Cuello, P., Cornejo, P., " +
				"Mignino, J., Ojeda, R.A. and Verzi, D.H. 2026. A new genus and species of octodontid rodent from the hilly " +
				"Chaco of central Argentina (Rodentia: Octodontidae). Vertebrate Zoology 76:361-380. doi:10.3897/vz.76.e187462",
		},
		{
			OldName: "Dactylopsila kambuayai", NewName: "Dactylonax kambuayai",
			Comment: "moved from Dactylopsila to Dactylonax", Category: "genus change",
			Reference: "Flannery, T.F., Aplin, K.P., Bocos, C., Koungoulos, L.G. and Helgen, K.M. 2026. Found alive after " +
				"6,000 years: modern records of an 'extinct' Papuan marsupial, Dactylonax kambuayai (Marsupialia: Petauridae), " +
				"with a revision of the systematics and zoogeography of the genus Dactylonax. Records of the Australian Museum " +
				"78(1):17-34. doi:10.3853/j.2201-4349.78.2026.3003",
		},
		{
			OldName: "Ctenomys minutus", NewName: "Ctenomys brasiliensis",
			Comment: "lumped into C. brasiliensis", Category: "lump",
			Reference: "Maestri, R., Gonçalves, G.L., Nicolas, V., Bryjova, A., Fornel, R., Coissac, E., Taberlet, P.,  " +
				"Moreira, G.R.P., and Freitas, T.R. 2026. Ancient DNA reveals the identity and geographic origin of the " +
				"syntype of Ctenomys brasiliensis, the type species of the genus Ctenomys. Journal of Mammalogy 107(1):197-209.",
		},
		{
			OldName: "Arvicola italicus", NewName: "Arvicola destructor",
			Comment: "name changed to reflect priority", Category: "name change",
			Reference: "Zijlstra, J.S. 2026. Species before subspecies: the names of mammal species first named as " +
				"subspecies. Journal of Mammalogy (in press). doi:10.1093/jmammal/gyag049",
		},
	}

	return &mdd.Dataset{
		Release: mdd.Release{
			Version: "v2.5",
			Date:    "2026-07-28",
			Citation: "Mammal Diversity Database. (2026). Mammal Diversity Database (Version 2.5) [Data set]. Zenodo. " +
				"https://doi.org/10.5281/zenodo.21654811",
			Remarks: "This is an incremental release that documents 6,904 total species, of which 113 are recently extinct " +
				"(same as previous version) and 6,791 are extant (17 domestic extant, 6,774 wild extant).",
			ETag:        `"sample-etag-v2.5"`,
			Species:     len(species),
			LoadedAt:    LoadedAt,
			SourceURL:   mdd.DefaultURL,
			PrevVersion: "v2.4",
		},
		Species: species,
		Changes: changes,
	}
}
