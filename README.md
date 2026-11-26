# F1 Predictor

Site web (HTML / CSS / JavaScript simple) + serveur **Go** (bibliothèque standard uniquement) qui interroge l'API [Jolpica F1](https://api.jolpi.ca/) pour prédire le classement d'une course.

## Lancer

```bash
go run .
# puis ouvrir http://localhost:8080
```

Variables optionnelles : `PORT` (défaut 8080), `JOLPICA_BASE` (défaut `https://api.jolpi.ca/ergast/f1`).
Tests : `go test ./...` (utilisent un faux serveur Jolpica, aucun accès réseau requis).

## Utilisation

1. Choisis le **circuit** (calendrier de la dernière saison).
2. Coche les **pilotes** et change si tu veux leur **voiture** (écurie).
3. Clique sur **Prédire** : classement, probabilité de victoire indicative, détail et niveau de fiabilité.

## Comment la prédiction est calculée (`predict.go`)

On sépare la voiture du pilote :

- **Voiture** : position moyenne de l'écurie, mélange de la forme récente (2 dernières saisons, les courses récentes comptent plus) et de l'historique sur ce circuit (8 saisons, poids x0,6 par année de recul).
- **Pilote** : écart moyen avec la moyenne de son écurie dans la même course (forme + circuit). Comparer au coéquipier retire l'effet de la voiture, donc un pilote mis dans une autre voiture garde son "niveau".
- **Score** = position attendue de la voiture + écart du pilote (plus bas = mieux).

Les constantes (demi-vie, poids, lissage) sont en haut de `predict.go`.

## Limites

- Modèle simple et explicable, pas du machine learning : pas de météo, qualifications, mises à jour de voiture, pénalités.
- L'historique circuit d'une voiture utilise des positions brutes, donc un ancien dominateur peut être surévalué (atténué par le poids décroissant et le lissage vers la forme actuelle).
- L'API publique Jolpica limite le débit : le serveur limite ses requêtes (~3/s) et met tout en cache (30 min pour la saison en cours, 24 h pour le passé). La première prédiction sur un circuit prend quelques secondes.

## Structure

- `main.go` : serveur HTTP, endpoints `GET /api/meta` et `POST /api/predict`, fichiers statiques embarqués
- `jolpica.go` : client API (pagination, limiteur, nouvelles tentatives, cache)
- `predict.go` : calcul de la prédiction
- `web/` : `index.html`, `style.css`, `app.js`
