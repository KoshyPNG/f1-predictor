package main

import (
	"fmt"
	"math"
	"sort"
)

// Paramètres du modèle (lisibles et faciles à ajuster).
const (
	formHalfLife = 10.0 // une course vieille de 10 courses compte moitié moins
	yearDecay    = 0.6  // une saison de plus sur le circuit => poids x0.6
	kCircuit     = 2.0  // "poids a priori" : tire l'historique circuit vers la forme actuelle
	kDeltaForm   = 3.0  // idem pour l'écart pilote/coéquipier (prior = 0)
	fieldMean    = 10.5 // position moyenne par défaut si aucune donnée
	temperature  = 2.0  // adoucit la probabilité de victoire (softmax)
)

// Entry : un pilote assigné à une voiture (écurie) choisie par l'utilisateur.
type Entry struct {
	DriverID      string `json:"driverId"`
	ConstructorID string `json:"constructorId"`
}

type Prediction struct {
	Rank            int      `json:"rank"`
	DriverID        string   `json:"driverId"`
	DriverName      string   `json:"driverName"`
	DriverCode      string   `json:"driverCode"`
	ConstructorID   string   `json:"constructorId"`
	ConstructorName string   `json:"constructorName"`
	Score           float64  `json:"score"`   // position attendue (plus bas = mieux)
	WinProb         float64  `json:"winProb"` // 0..1, indicatif
	FormPos         *float64 `json:"formPos"` // position moyenne récente du pilote
	CarForm         float64  `json:"carForm"` // position moyenne récente de la voiture
	CarCircuit      *float64 `json:"carCircuit"`
	CircuitPos      *float64 `json:"circuitPos"` // position moyenne du pilote sur ce circuit
	CircuitRaces    int      `json:"circuitRaces"`
	DriverDelta     float64  `json:"driverDelta"` // < 0 : meilleur que la voiture ne l'indique
	DNFRate         float64  `json:"dnfRate"`
	FormRaces       int      `json:"formRaces"`
	Confidence      string   `json:"confidence"` // low | medium | high
}

// userError : erreur imputable à la requête (HTTP 400).
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

type raceKey struct{ season, round int }

// wavg accumule une moyenne pondérée.
type wavg struct {
	sw, swx float64
	n       int
}

func (a *wavg) add(w, x float64) { a.sw += w; a.swx += w * x; a.n++ }
func (a wavg) mean() (float64, bool) {
	if a.sw == 0 {
		return 0, false
	}
	return a.swx / a.sw, true
}

// shrink : moyenne pondérée tirée vers prior avec un poids k.
func (a wavg) shrink(prior, k float64) float64 { return (a.swx + k*prior) / (a.sw + k) }

// teamMeans : position moyenne de chaque écurie dans chaque course.
func teamMeans(rows []Row) map[raceKey]map[string]float64 {
	sum := map[raceKey]map[string]float64{}
	cnt := map[raceKey]map[string]int{}
	for _, r := range rows {
		k := raceKey{r.Season, r.Round}
		if sum[k] == nil {
			sum[k] = map[string]float64{}
			cnt[k] = map[string]int{}
		}
		sum[k][r.ConstructorID] += float64(r.Pos)
		cnt[k][r.ConstructorID]++
	}
	for k, m := range sum {
		for c := range m {
			m[c] /= float64(cnt[k][c])
		}
	}
	return sum
}

// Predict classe les engagés.
//
// Idée : on sépare la performance de la VOITURE de celle du PILOTE.
//   - voiture : position moyenne de l'écurie (forme récente + historique sur ce
//     circuit, les courses récentes comptant davantage) ;
//   - pilote  : écart moyen avec la moyenne de son écurie dans la même course
//     (forme récente + historique sur ce circuit). Comparer au coéquipier retire
//     l'effet de la voiture, donc on peut mettre le pilote dans une autre voiture.
//
// score = position attendue de la voiture + écart du pilote. Plus bas = mieux.
func Predict(form, circuit []Row, entries []Entry, refSeason int) ([]Prediction, error) {
	if len(entries) < 2 {
		return nil, userError{"Sélectionne au moins 2 pilotes."}
	}

	type info struct{ name, code string }
	drivers := map[string]info{}
	constructors := map[string]string{}
	for _, r := range form {
		drivers[r.DriverID] = info{r.DriverName, r.DriverCode}
		constructors[r.ConstructorID] = r.ConstructorName
	}

	// ancienneté des courses récentes (0 = la plus récente)
	seen := map[raceKey]bool{}
	var keys []raceKey
	for _, r := range form {
		k := raceKey{r.Season, r.Round}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].season != keys[j].season {
			return keys[i].season < keys[j].season
		}
		return keys[i].round < keys[j].round
	})
	age := map[raceKey]int{}
	for i, k := range keys {
		age[k] = len(keys) - 1 - i
	}
	formW := func(k raceKey) float64 { return math.Pow(0.5, float64(age[k])/formHalfLife) }
	circW := func(season int) float64 {
		d := refSeason - season
		if d < 0 {
			d = 0
		}
		return math.Pow(yearDecay, float64(d))
	}

	// --- forme récente ---
	formTeam := teamMeans(form)
	carForm := map[string]*wavg{}
	for k, m := range formTeam {
		for c, pos := range m {
			if carForm[c] == nil {
				carForm[c] = &wavg{}
			}
			carForm[c].add(formW(k), pos)
		}
	}
	drvPos := map[string]*wavg{}
	drvDelta := map[string]*wavg{}
	dnf := map[string]int{}
	for _, r := range form {
		k := raceKey{r.Season, r.Round}
		w := formW(k)
		if drvPos[r.DriverID] == nil {
			drvPos[r.DriverID], drvDelta[r.DriverID] = &wavg{}, &wavg{}
		}
		drvPos[r.DriverID].add(w, float64(r.Pos))
		drvDelta[r.DriverID].add(w, float64(r.Pos)-formTeam[k][r.ConstructorID])
		if !r.Finished {
			dnf[r.DriverID]++
		}
	}

	// --- historique sur le circuit ---
	circTeam := teamMeans(circuit)
	carCirc := map[string]*wavg{}
	for k, m := range circTeam {
		for c, pos := range m {
			if carCirc[c] == nil {
				carCirc[c] = &wavg{}
			}
			carCirc[c].add(circW(k.season), pos)
		}
	}
	drvCircPos := map[string]*wavg{}
	drvCircDelta := map[string]*wavg{}
	for _, r := range circuit {
		k := raceKey{r.Season, r.Round}
		w := circW(r.Season)
		if drvCircPos[r.DriverID] == nil {
			drvCircPos[r.DriverID], drvCircDelta[r.DriverID] = &wavg{}, &wavg{}
		}
		drvCircPos[r.DriverID].add(w, float64(r.Pos))
		drvCircDelta[r.DriverID].add(w, float64(r.Pos)-circTeam[k][r.ConstructorID])
	}

	// --- score de chaque engagé ---
	out := make([]Prediction, 0, len(entries))
	used := map[string]bool{}
	for _, e := range entries {
		d, ok := drivers[e.DriverID]
		if !ok {
			return nil, userError{fmt.Sprintf("Pilote inconnu : %q", e.DriverID)}
		}
		cname, ok := constructors[e.ConstructorID]
		if !ok {
			return nil, userError{fmt.Sprintf("Voiture inconnue : %q", e.ConstructorID)}
		}
		if used[e.DriverID] {
			return nil, userError{fmt.Sprintf("Pilote en double : %s", d.name)}
		}
		used[e.DriverID] = true

		// voiture
		carFormVal := fieldMean
		if a := carForm[e.ConstructorID]; a != nil {
			if m, ok := a.mean(); ok {
				carFormVal = m
			}
		}
		carScore := carFormVal
		var carCircPtr *float64
		if a := carCirc[e.ConstructorID]; a != nil && a.sw > 0 {
			carScore = a.shrink(carFormVal, kCircuit)
			m, _ := a.mean()
			carCircPtr = &m
		}

		// pilote
		formDelta := 0.0
		if a := drvDelta[e.DriverID]; a != nil {
			formDelta = a.shrink(0, kDeltaForm)
		}
		delta := formDelta
		if a := drvCircDelta[e.DriverID]; a != nil && a.sw > 0 {
			delta = a.shrink(formDelta, kCircuit)
		}

		p := Prediction{
			DriverID: e.DriverID, DriverName: d.name, DriverCode: d.code,
			ConstructorID: e.ConstructorID, ConstructorName: cname,
			Score: carScore + delta, CarForm: carFormVal, CarCircuit: carCircPtr, DriverDelta: delta,
		}
		if a := drvPos[e.DriverID]; a != nil {
			if m, ok := a.mean(); ok {
				p.FormPos = &m
			}
			p.FormRaces = a.n
			p.DNFRate = float64(dnf[e.DriverID]) / float64(a.n)
		}
		if a := drvCircPos[e.DriverID]; a != nil {
			if m, ok := a.mean(); ok {
				p.CircuitPos = &m
			}
			p.CircuitRaces = a.n
		}
		switch {
		case p.FormRaces >= 15 && p.CircuitRaces >= 2:
			p.Confidence = "high"
		case p.FormRaces >= 6:
			p.Confidence = "medium"
		default:
			p.Confidence = "low"
		}
		out = append(out, p)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score < out[j].Score })

	// probabilité de victoire indicative : softmax sur -score
	best := out[0].Score
	var total float64
	for i := range out {
		out[i].WinProb = math.Exp(-(out[i].Score - best) / temperature)
		total += out[i].WinProb
	}
	for i := range out {
		out[i].WinProb /= total
		out[i].Rank = i + 1
	}
	return out, nil
}
