package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------- modèle ----------

func mk(season, round int, circuit, drv, cons string, pos int) Row {
	return Row{Season: season, Round: round, CircuitID: circuit, DriverID: drv, DriverName: strings.ToUpper(drv),
		DriverCode: strings.ToUpper(drv[:3]), ConstructorID: cons, ConstructorName: cons, Pos: pos, Finished: true}
}

// Deux écuries : "top" (P1/P2) et "low" (P3/P4). ace bat son coéquipier chaque fois.
func formRows() []Row {
	var rows []Row
	for r := 1; r <= 8; r++ {
		rows = append(rows,
			mk(2026, r, "x", "ace", "top", 1), mk(2026, r, "x", "bob", "top", 2),
			mk(2026, r, "x", "cat", "low", 3), mk(2026, r, "x", "dan", "low", 4))
	}
	return rows
}

func TestPredictOrder(t *testing.T) {
	got, err := Predict(formRows(), nil, []Entry{
		{"dan", "low"}, {"bob", "top"}, {"ace", "top"}, {"cat", "low"},
	}, 2026)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.DriverID)
	}
	if strings.Join(ids, ",") != "ace,bob,cat,dan" {
		t.Fatalf("ordre = %v", ids)
	}
	var sum float64
	for _, p := range got {
		sum += p.WinProb
	}
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("les probabilités doivent sommer à 1, got %v", sum)
	}
}

func TestPredictSwapCar(t *testing.T) {
	// dan dans la voiture de tête doit passer devant cat resté dans la voiture lente.
	got, err := Predict(formRows(), nil, []Entry{{"dan", "top"}, {"cat", "low"}}, 2026)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].DriverID != "dan" {
		t.Fatalf("dan en voiture de tête devrait gagner, got %s", got[0].DriverID)
	}
}

func TestPredictCircuitHistory(t *testing.T) {
	// bob et cat sont proches en forme, mais bob a toujours dominé ce circuit.
	var circ []Row
	for s := 2020; s <= 2025; s++ {
		circ = append(circ, mk(s, 5, "y", "bob", "top", 1), mk(s, 5, "y", "ace", "top", 9), mk(s, 5, "y", "cat", "low", 2))
	}
	without, _ := Predict(formRows(), nil, []Entry{{"ace", "top"}, {"bob", "top"}}, 2026)
	with, _ := Predict(formRows(), circ, []Entry{{"ace", "top"}, {"bob", "top"}}, 2026)
	if without[0].DriverID != "ace" {
		t.Fatalf("sans historique ace devrait mener")
	}
	if with[0].DriverID != "bob" {
		t.Fatalf("avec l'historique circuit bob devrait passer devant, got %s", with[0].DriverID)
	}
}

func TestPredictValidation(t *testing.T) {
	cases := map[string][]Entry{
		"un seul": {{"ace", "top"}},
		"inconnu": {{"ace", "top"}, {"zzz", "top"}},
		"voiture": {{"ace", "top"}, {"bob", "nope"}},
		"doublon": {{"ace", "top"}, {"ace", "low"}},
	}
	for name, e := range cases {
		if _, err := Predict(formRows(), nil, e, 2026); err == nil {
			t.Errorf("%s : erreur attendue", name)
		} else if _, ok := err.(userError); !ok {
			t.Errorf("%s : userError attendue, got %T", name, err)
		}
	}
}

// ---------- faux serveur Jolpica + API complète ----------

type fakeRes struct {
	season, round      int
	circuit, drv, cons string
	pos                int
	status             string
}

func fakeData() (season map[int][]fakeRes) {
	y := time.Now().Year()
	season = map[int][]fakeRes{}
	drivers := []struct{ id, cons string }{{"ver", "red_bull"}, {"per", "red_bull"}, {"lec", "ferrari"}, {"sai", "ferrari"}, {"alo", "aston_martin"}, {"str", "aston_martin"}}
	for _, s := range []int{y - 1, y} {
		for round := 1; round <= 4; round++ {
			circ := []string{"monza", "spa"}[round%2]
			for i, d := range drivers {
				st := "Finished"
				if round == 3 && d.id == "str" {
					st = "Engine"
				}
				season[s] = append(season[s], fakeRes{s, round, circ, d.id, d.cons, i + 1, st})
			}
		}
	}
	return season
}

func resultsJSON(rows []fakeRes, offset, limit int) map[string]any {
	total := len(rows)
	end := offset + limit
	if offset > total {
		offset = total
	}
	if end > total {
		end = total
	}
	page := rows[offset:end]
	var races []map[string]any
	idx := map[string]int{}
	for _, r := range page {
		k := fmt.Sprintf("%d-%d", r.season, r.round)
		i, ok := idx[k]
		if !ok {
			races = append(races, map[string]any{
				"season": strconv.Itoa(r.season), "round": strconv.Itoa(r.round),
				"Circuit": map[string]any{"circuitId": r.circuit}, "Results": []map[string]any{},
			})
			i = len(races) - 1
			idx[k] = i
		}
		races[i]["Results"] = append(races[i]["Results"].([]map[string]any), map[string]any{
			"position": strconv.Itoa(r.pos), "status": r.status,
			"Driver":      map[string]any{"driverId": r.drv, "code": strings.ToUpper(r.drv), "givenName": "G", "familyName": r.drv},
			"Constructor": map[string]any{"constructorId": r.cons, "name": r.cons},
		})
	}
	return map[string]any{"MRData": map[string]any{"total": strconv.Itoa(total), "RaceTable": map[string]any{"Races": races}}}
}

func newFake(t *testing.T) *httptest.Server {
	data := fakeData()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/ergast/f1"), "/"), "/")
		season, _ := strconv.Atoi(parts[0])
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 || limit > 100 {
			limit = 100
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case len(parts) == 2 && parts[1] == "results.json":
			json.NewEncoder(w).Encode(resultsJSON(data[season], offset, limit))
		case len(parts) == 2 && parts[1] == "circuits.json":
			json.NewEncoder(w).Encode(map[string]any{"MRData": map[string]any{"CircuitTable": map[string]any{"Circuits": []map[string]any{
				{"circuitId": "monza", "circuitName": "Monza", "Location": map[string]any{"locality": "Monza", "country": "Italy"}},
				{"circuitId": "spa", "circuitName": "Spa", "Location": map[string]any{"locality": "Spa", "country": "Belgium"}},
			}}}})
		case len(parts) == 4 && parts[1] == "circuits" && parts[3] == "results.json":
			var rows []fakeRes
			for _, x := range data[season] {
				if x.circuit == parts[2] {
					rows = append(rows, x)
				}
			}
			json.NewEncoder(w).Encode(resultsJSON(rows, offset, limit))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPaginationAndAPI(t *testing.T) {
	fake := newFake(t)
	defer fake.Close()
	// limite de 100 lignes/page : on force plusieurs pages en gonflant une saison
	data := fakeData()
	_ = data

	s := &Server{client: NewClient(fake.URL+"/ergast/f1", time.Millisecond)}
	app := httptest.NewServer(s.routes())
	defer app.Close()

	// /api/meta
	resp, err := http.Get(app.URL + "/api/meta")
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(meta.Drivers) != 6 || len(meta.Constructors) != 3 || len(meta.Circuits) != 2 {
		t.Fatalf("meta inattendu : status=%d %+v", resp.StatusCode, meta)
	}

	post := func(body string) (int, map[string]any) {
		r, err := http.Post(app.URL+"/api/predict", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out map[string]any
		json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	// prédiction valide, avec une voiture échangée (alo -> red_bull)
	code, out := post(`{"circuitId":"monza","entries":[{"driverId":"ver","constructorId":"red_bull"},{"driverId":"lec","constructorId":"ferrari"},{"driverId":"alo","constructorId":"red_bull"},{"driverId":"str","constructorId":"aston_martin"}]}`)
	if code != 200 {
		t.Fatalf("predict status=%d %v", code, out)
	}
	preds := out["predictions"].([]any)
	if len(preds) != 4 {
		t.Fatalf("4 prédictions attendues, got %d", len(preds))
	}
	top := preds[0].(map[string]any)
	if top["driverId"] != "ver" && top["driverId"] != "alo" {
		t.Fatalf("une Red Bull devrait mener : %v", top)
	}
	if preds[3].(map[string]any)["driverId"] != "str" {
		t.Fatalf("str (aston, dernier) devrait finir dernier : %v", preds[3])
	}

	// validations
	if c, _ := post(`{"circuitId":"../../etc","entries":[{"driverId":"ver","constructorId":"red_bull"},{"driverId":"lec","constructorId":"ferrari"}]}`); c != 400 {
		t.Errorf("circuit invalide : 400 attendu, got %d", c)
	}
	if c, _ := post(`{"circuitId":"monza","entries":[{"driverId":"ver","constructorId":"red_bull"}]}`); c != 400 {
		t.Errorf("1 seul pilote : 400 attendu, got %d", c)
	}
	if c, _ := post(`pas du json`); c != 400 {
		t.Errorf("JSON invalide : 400 attendu, got %d", c)
	}

	// le front statique est servi
	r, _ := http.Get(app.URL + "/")
	if r.StatusCode != 200 {
		t.Errorf("index.html non servi : %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestFetchResultsPagination(t *testing.T) {
	// 250 lignes => 3 pages de 100
	var rows []fakeRes
	for i := 0; i < 250; i++ {
		rows = append(rows, fakeRes{2025, i/20 + 1, "monza", fmt.Sprintf("d%d", i%20), "t", i%20 + 1, "Finished"})
	}
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		json.NewEncoder(w).Encode(resultsJSON(rows, offset, 100))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, time.Millisecond)
	got, err := c.fetchResults(t.Context(), "/2025/results.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 250 || pages != 3 {
		t.Fatalf("rows=%d pages=%d (attendu 250 / 3)", len(got), pages)
	}
}
