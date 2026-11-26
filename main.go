package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"time"
)

//go:embed web
var webFS embed.FS

const historySeasons = 8 // nombre de saisons d'historique sur le circuit

type Server struct {
	client *Client
}

type DriverInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Code          string `json:"code"`
	ConstructorID string `json:"constructorId"`
}

type ConstructorInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Meta struct {
	Season       int               `json:"season"`
	Circuits     []Circuit         `json:"circuits"`
	Drivers      []DriverInfo      `json:"drivers"`
	Constructors []ConstructorInfo `json:"constructors"`
}

// loadForm charge la dernière saison disposant de résultats + la précédente.
func (s *Server) loadForm(ctx context.Context) (int, []Row, error) {
	y := time.Now().Year()
	latest := 0
	var cur []Row
	for yr := y; yr >= y-2; yr-- {
		r, err := s.client.SeasonResults(ctx, yr)
		if err != nil {
			return 0, nil, err
		}
		if len(r) > 0 {
			latest, cur = yr, r
			break
		}
	}
	if latest == 0 {
		return 0, nil, errors.New("aucun résultat trouvé sur l'API")
	}
	prev, err := s.client.SeasonResults(ctx, latest-1)
	if err != nil {
		return 0, nil, err
	}
	all := make([]Row, 0, len(prev)+len(cur))
	all = append(all, prev...)
	all = append(all, cur...)
	return latest, all, nil
}

func (s *Server) buildMeta(ctx context.Context) (*Meta, []Row, error) {
	latest, form, err := s.loadForm(ctx)
	if err != nil {
		return nil, nil, err
	}
	circuits, err := s.client.Circuits(ctx, latest)
	if err == nil && len(circuits) == 0 {
		circuits, err = s.client.Circuits(ctx, latest-1)
	}
	if err != nil {
		return nil, nil, err
	}

	// pilotes de la dernière saison, avec leur voiture la plus récente
	type last struct {
		round int
		d     DriverInfo
	}
	byDriver := map[string]last{}
	consts := map[string]string{}
	for _, r := range form {
		if r.Season != latest {
			continue
		}
		consts[r.ConstructorID] = r.ConstructorName
		if l, ok := byDriver[r.DriverID]; !ok || r.Round >= l.round {
			byDriver[r.DriverID] = last{r.Round, DriverInfo{ID: r.DriverID, Name: r.DriverName, Code: r.DriverCode, ConstructorID: r.ConstructorID}}
		}
	}
	m := &Meta{Season: latest, Circuits: circuits}
	for _, l := range byDriver {
		m.Drivers = append(m.Drivers, l.d)
	}
	for id, name := range consts {
		m.Constructors = append(m.Constructors, ConstructorInfo{ID: id, Name: name})
	}
	sort.Slice(m.Drivers, func(i, j int) bool {
		if m.Drivers[i].ConstructorID != m.Drivers[j].ConstructorID {
			return m.Drivers[i].ConstructorID < m.Drivers[j].ConstructorID
		}
		return m.Drivers[i].Name < m.Drivers[j].Name
	})
	sort.Slice(m.Constructors, func(i, j int) bool { return m.Constructors[i].Name < m.Constructors[j].Name })
	return m, form, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ue userError
	if errors.As(err, &ue) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ue.msg})
		return
	}
	log.Printf("erreur : %v", err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Impossible de récupérer les données Jolpica pour l'instant. Réessaie dans un moment."})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
	defer cancel()
	m, _, err := s.buildMeta(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handlePredict(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CircuitID string  `json:"circuitId"`
		Entries   []Entry `json:"entries"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, userError{"Requête invalide."})
		return
	}
	if len(req.Entries) < 2 || len(req.Entries) > 30 {
		writeErr(w, userError{"Choisis entre 2 et 30 pilotes."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
	defer cancel()
	meta, form, err := s.buildMeta(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	var circuit *Circuit
	for i := range meta.Circuits {
		if meta.Circuits[i].ID == req.CircuitID {
			circuit = &meta.Circuits[i]
		}
	}
	if circuit == nil { // valide aussi l'identifiant avant de l'utiliser dans une URL
		writeErr(w, userError{"Circuit inconnu."})
		return
	}
	history, err := s.client.CircuitHistory(ctx, circuit.ID, meta.Season-historySeasons, meta.Season)
	if err != nil {
		writeErr(w, err)
		return
	}
	preds, err := Predict(form, history, req.Entries, meta.Season)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"circuit":     circuit,
		"season":      meta.Season,
		"history":     fmt.Sprintf("%d–%d", meta.Season-historySeasons, meta.Season),
		"predictions": preds,
	})
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("POST /api/predict", s.handlePredict)
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	base := getenv("JOLPICA_BASE", "https://api.jolpi.ca/ergast/f1")
	addr := ":" + getenv("PORT", "8080")
	s := &Server{client: NewClient(base, 300*time.Millisecond)}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	log.Printf("F1 Predictor sur http://localhost%s (API : %s)", addr, base)
	log.Fatal(srv.ListenAndServe())
}
