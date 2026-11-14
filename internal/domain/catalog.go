package domain

// Areas and categories mirror sewain-web/src/lib/data/{areas,categories}.ts (only what the rules need).

type Area struct {
	ID   string
	Name string
	Lat  float64
	Lng  float64
}

var Areas = map[string]Area{
	"ugm":          {"ugm", "Kampus UGM", -7.7713, 110.3776},
	"pogung":       {"pogung", "Pogung", -7.7596, 110.3727},
	"sendowo":      {"sendowo", "Sendowo", -7.7652, 110.3752},
	"karangmalang": {"karangmalang", "Karangmalang", -7.7738, 110.3858},
	"karangwuni":   {"karangwuni", "Karangwuni", -7.7662, 110.3812},
	"klebengan":    {"klebengan", "Klebengan", -7.7684, 110.3853},
	"terban":       {"terban", "Terban", -7.7779, 110.3731},
	"condongcatur": {"condongcatur", "Condongcatur", -7.7571, 110.3949},
}

// CategoryRisk: high-risk categories (PRD 12.3). Vehicles need manual approval, odometer/BBM photos, etc.
var CategoryRisk = map[string]string{
	"kamera":     "elektronik",
	"elektronik": "elektronik",
	"alat-masak": "listrik",
	"kendaraan":  "kendaraan",
	"olahraga":   "keselamatan",
}

var CategoryNames = map[string]string{
	"camping": "Camping & Hiking", "kamera": "Kamera & Foto", "pakaian-acara": "Pakaian Acara", "elektronik": "Elektronik & Presentasi",
	"alat-masak": "Alat Masak", "musim-hujan": "Musim Hujan", "kendaraan": "Kendaraan", "olahraga": "Olahraga",
}

func IsVehicle(category string) bool { return CategoryRisk[category] == "kendaraan" }
