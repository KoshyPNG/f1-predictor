package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Row est une ligne de résultat de course (un pilote dans une course).
type Row struct {
	Season          int
	Round           int
	CircuitID       string
	DriverID        string
	DriverName      string
	DriverCode      string
	ConstructorID   string
	ConstructorName string
	Pos             int  // position finale classée (1 = vainqueur)
	Finished        bool // a vu l'arrivée (ou été classé avec des tours de retard)
}

// Circuit décrit un circuit du calendrier.
type Circuit struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Locality string `json:"locality"`
	Country  string `json:"country"`
}

type cacheEntry struct {
	mu  sync.Mutex
	val any
	exp time.Time
}

// Client interroge l'API Jolpica avec un limiteur de débit (l'API publique
// limite à quelques requêtes par seconde) et un cache mémoire.
type Client struct {
	base  string
	hc    *http.Client
	tick  *time.Ticker
	mu    sync.Mutex
	cache map[string]*cacheEntry
}

func NewClient(base string, minInterval time.Duration) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		hc:    &http.Client{Timeout: 20 * time.Second},
		tick:  time.NewTicker(minInterval),
		cache: map[string]*cacheEntry{},
	}
}

// cached renvoie la valeur en cache ou appelle fn. Les appels concurrents
// pour une même clé attendent le premier au lieu de refaire la requête.
func cached[T any](c *Client, key string, ttl time.Duration, fn func() (T, error)) (T, error) {
	c.mu.Lock()
	e, ok := c.cache[key]
	if !ok {
		e = &cacheEntry{}
		c.cache[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := e.val.(T); ok && time.Now().Before(e.exp) {
		return v, nil
	}
	v, err := fn()
	if err != nil {
		var zero T
		return zero, err
	}
	e.val, e.exp = v, time.Now().Add(ttl)
	return v, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// get fait un GET JSON avec nouvelle tentative sur 429 / 5xx.
func (c *Client) get(ctx context.Context, path string, out any) error {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		select {
		case <-c.tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "f1-predictor/1.0 (projet perso)")
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			if err := sleepCtx(ctx, time.Duration(attempt+1)*time.Second); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			wait := time.Duration(attempt+1) * 2 * time.Second
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 && ra <= 15 {
				wait = time.Duration(ra) * time.Second
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("jolpica: HTTP %d", resp.StatusCode)
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("jolpica: HTTP %d pour %s", resp.StatusCode, path)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
		resp.Body.Close()
		return err
	}
	return lastErr
}

type resultsResp struct {
	MRData struct {
		Total     string `json:"total"`
		RaceTable struct {
			Races []struct {
				Season  string `json:"season"`
				Round   string `json:"round"`
				Circuit struct {
					CircuitID string `json:"circuitId"`
				} `json:"Circuit"`
				Results []struct {
					Position string `json:"position"`
					Status   string `json:"status"`
					Driver   struct {
						DriverID   string `json:"driverId"`
						Code       string `json:"code"`
						GivenName  string `json:"givenName"`
						FamilyName string `json:"familyName"`
					} `json:"Driver"`
					Constructor struct {
						ConstructorID string `json:"constructorId"`
						Name          string `json:"name"`
					} `json:"Constructor"`
				} `json:"Results"`
			} `json:"Races"`
		} `json:"RaceTable"`
	} `json:"MRData"`
}

func isFinished(status string) bool {
	return status == "Finished" || strings.HasPrefix(status, "+") || strings.Contains(status, "Lap")
}

// fetchResults parcourt toutes les pages (100 lignes max par requête).
func (c *Client) fetchResults(ctx context.Context, path string) ([]Row, error) {
	var rows []Row
	for offset := 0; ; offset += 100 {
		var resp resultsResp
		if err := c.get(ctx, fmt.Sprintf("%s?limit=100&offset=%d", path, offset), &resp); err != nil {
			return nil, err
		}
		for _, race := range resp.MRData.RaceTable.Races {
			season, _ := strconv.Atoi(race.Season)
			round, _ := strconv.Atoi(race.Round)
			for _, r := range race.Results {
				pos, err := strconv.Atoi(r.Position)
				if err != nil || pos < 1 {
					continue
				}
				rows = append(rows, Row{
					Season:          season,
					Round:           round,
					CircuitID:       race.Circuit.CircuitID,
					DriverID:        r.Driver.DriverID,
					DriverName:      strings.TrimSpace(r.Driver.GivenName + " " + r.Driver.FamilyName),
					DriverCode:      r.Driver.Code,
					ConstructorID:   r.Constructor.ConstructorID,
					ConstructorName: r.Constructor.Name,
					Pos:             pos,
					Finished:        isFinished(r.Status),
				})
			}
		}
		total, _ := strconv.Atoi(resp.MRData.Total)
		if offset+100 >= total {
			break
		}
	}
	return rows, nil
}

func ttlFor(season int) time.Duration {
	if season >= time.Now().Year() {
		return 30 * time.Minute // saison en cours : de nouveaux résultats peuvent arriver
	}
	return 24 * time.Hour
}

// SeasonResults : tous les résultats de course d'une saison.
func (c *Client) SeasonResults(ctx context.Context, season int) ([]Row, error) {
	return cached(c, fmt.Sprintf("season:%d", season), ttlFor(season), func() ([]Row, error) {
		return c.fetchResults(ctx, fmt.Sprintf("/%d/results.json", season))
	})
}

// CircuitResults : résultats sur un circuit donné pour une saison (vide si pas de course).
func (c *Client) CircuitResults(ctx context.Context, circuitID string, season int) ([]Row, error) {
	key := fmt.Sprintf("circuit:%s:%d", circuitID, season)
	return cached(c, key, ttlFor(season), func() ([]Row, error) {
		return c.fetchResults(ctx, fmt.Sprintf("/%d/circuits/%s/results.json", season, url.PathEscape(circuitID)))
	})
}

// CircuitHistory récupère en parallèle (le limiteur de débit reste global)
// les résultats d'un circuit entre deux saisons incluses.
func (c *Client) CircuitHistory(ctx context.Context, circuitID string, from, to int) ([]Row, error) {
	if from < 1950 {
		from = 1950
	}
	type res struct {
		rows []Row
		err  error
	}
	out := make([]res, to-from+1)
	var wg sync.WaitGroup
	for s := from; s <= to; s++ {
		wg.Add(1)
		go func(i, s int) {
			defer wg.Done()
			rows, err := c.CircuitResults(ctx, circuitID, s)
			out[i] = res{rows, err}
		}(s-from, s)
	}
	wg.Wait()
	var all []Row
	for _, r := range out {
		if r.err != nil {
			return nil, r.err
		}
		all = append(all, r.rows...)
	}
	return all, nil
}

// Circuits : calendrier d'une saison.
func (c *Client) Circuits(ctx context.Context, season int) ([]Circuit, error) {
	return cached(c, fmt.Sprintf("circuits:%d", season), ttlFor(season), func() ([]Circuit, error) {
		var resp struct {
			MRData struct {
				CircuitTable struct {
					Circuits []struct {
						CircuitID   string `json:"circuitId"`
						CircuitName string `json:"circuitName"`
						Location    struct {
							Locality string `json:"locality"`
							Country  string `json:"country"`
						} `json:"Location"`
					} `json:"Circuits"`
				} `json:"CircuitTable"`
			} `json:"MRData"`
		}
		if err := c.get(ctx, fmt.Sprintf("/%d/circuits.json?limit=100", season), &resp); err != nil {
			return nil, err
		}
		var out []Circuit
		for _, x := range resp.MRData.CircuitTable.Circuits {
			out = append(out, Circuit{ID: x.CircuitID, Name: x.CircuitName, Locality: x.Location.Locality, Country: x.Location.Country})
		}
		return out, nil
	})
}
