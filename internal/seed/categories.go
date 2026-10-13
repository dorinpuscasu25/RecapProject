package seed

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/nlp"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
)

func gazKey(s string) string { return nlp.EntityKey(s) }

type catSeed struct {
	slug, parent, ro, ru, en, icon string
	hints                          []string
}

// Moldova-focused starter taxonomy. Replaceable in full via admin APIs.
var categorySeeds = []catSeed{
	{"politics", "", "Politică", "Политика", "Politics", "landmark", []string{"parlament", "guvern", "deputat", "partid", "alegeri", "ministru", "președinte", "opoziție", "парламент", "правительство", "депутат", "партия", "выборы", "министр", "президент", "оппозиция"}},
	{"economy", "", "Economie", "Экономика", "Economy", "chart", []string{"economie", "inflație", "buget", "salariu", "pensii", "export", "import", "PIB", "investiții", "taxe", "экономика", "инфляция", "бюджет", "зарплата", "пенсии", "экспорт", "ВВП", "инвестиции", "налоги"}},
	{"justice", "", "Justiție", "Правосудие", "Justice", "scale", []string{"instanță", "judecător", "procuror", "dosar", "sentință", "condamnat", "corupție", "anticorupție", "суд", "судья", "прокурор", "дело", "приговор", "коррупция"}},
	{"education", "", "Educație", "Образование", "Education", "book", []string{"școală", "elevi", "profesori", "universitate", "studenți", "bacalaureat", "școală", "школа", "ученики", "учителя", "университет", "студенты", "бакалавриат"}},
	{"health", "", "Sănătate", "Здоровье", "Health", "heart", []string{"spital", "medici", "pacienți", "vaccin", "boală", "sănătate", "CNAM", "больница", "врачи", "пациенты", "вакцина", "болезнь", "здравоохранение"}},
	{"technology", "", "Tehnologie", "Технологии", "Technology", "chip", []string{"tehnologie", "IT", "digital", "internet", "inteligență artificială", "startup", "software", "cibernetic", "технологии", "цифровой", "интернет", "искусственный интеллект", "стартап"}},
	{"agriculture", "", "Agricultură", "Сельское хозяйство", "Agriculture", "wheat", []string{"agricultură", "fermieri", "recoltă", "struguri", "cereale", "subvenții agricole", "irigare", "сельское хозяйство", "фермеры", "урожай", "виноград", "зерно", "субсидии"}},
	{"energy", "", "Energie", "Энергетика", "Energy", "bolt", []string{"energie", "gaze naturale", "tarif", "ANRE", "Energocom", "Moldovagaz", "centrala", "энергетика", "газ", "тариф", "НАРЭ", "электростанция"}},
	{"electricity-prices", "energy", "Prețul energiei electrice", "Цены на электроэнергию", "Electricity prices", "plug", []string{"energie electrică", "energia electrică", "curent electric", "kilowatt", "kWh", "tariful la energie", "электроэнергия", "киловатт", "тариф на электроэнергию"}},
	{"security", "", "Securitate", "Безопасность", "Security", "shield", []string{"securitate", "armată", "apărare", "poliție", "SIS", "frontieră", "dezinformare", "безопасность", "армия", "оборона", "полиция", "граница", "дезинформация"}},
	{"culture", "", "Cultură", "Культура", "Culture", "theater", []string{"cultură", "teatru", "festival", "muzică", "film", "muzeu", "carte", "культура", "театр", "фестиваль", "музыка", "кино", "музей"}},
	{"sport", "", "Sport", "Спорт", "Sport", "ball", []string{"fotbal", "meci", "campionat", "echipa națională", "olimpic", "sportiv", "футбол", "матч", "чемпионат", "сборная", "олимпийский", "спортсмен"}},
	{"chisinau-local", "", "Chișinău / Local", "Кишинёв / Местные", "Chișinău / Local", "city", []string{"Chișinău", "primăria", "consiliul municipal", "transport public", "troleibuz", "sector", "Кишинев", "примэрия", "муниципальный совет", "троллейбус"}},
	{"eu-integration", "", "Integrare europeană", "Европейская интеграция", "EU integration", "stars", []string{"Uniunea Europeană", "aderare", "integrare europeană", "negocieri de aderare", "Bruxelles", "Евросоюз", "вступление в ЕС", "евроинтеграция", "Брюссель"}},
}

func categories(ctx context.Context, tx pgx.Tx) error {
	idBySlug := map[string]any{}
	for i, c := range categorySeeds {
		names, _ := json.Marshal(map[string]string{"ro": c.ro, "ru": c.ru, "en": c.en})
		var parent any
		if c.parent != "" {
			parent = idBySlug[c.parent]
		}
		var id any
		if err := tx.QueryRow(ctx, `INSERT INTO categories (id, slug, parent_id, names, icon_key, display_order, classification_hints)
			VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (slug) DO UPDATE SET slug=EXCLUDED.slug RETURNING id`,
			ids.New(), c.slug, parent, names, c.icon, i*10, c.hints).Scan(&id); err != nil {
			return err
		}
		idBySlug[c.slug] = id
	}
	_, err := tx.Exec(ctx, `INSERT INTO taxonomy_versions (summary) VALUES ('seed')`)
	return err
}
