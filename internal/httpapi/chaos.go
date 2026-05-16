package httpapi

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// chaosEnabled est lu à l'init. Évite d'exposer les endpoints destructifs
// (panic/crash/memleak) sans opt-in explicite via DEMO_CHAOS_ENABLED=1.
var chaosEnabled = os.Getenv("DEMO_CHAOS_ENABLED") == "1"

// memHold conserve les blocs alloués par /api/memleak pour empêcher le GC
// de les libérer ; permet de démontrer les limites mémoire et l'OOMKill.
var (
	memHoldMu sync.Mutex
	memHold   [][]byte
)

// slowHandler simule une latence côté serveur (sleep, pas de CPU).
// Utile pour exercer les seuils de latence p95/p99 sans saturer le HPA CPU.
func slowHandler(w http.ResponseWriter, r *http.Request) {
	ms := atoiClamp(r.URL.Query().Get("ms"), 0, 10_000)
	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
	case <-r.Context().Done():
		http.Error(w, "client gone", 499)
		return
	}
	fmt.Fprintf(w, "slept %dms\n", ms)
}

// flakyHandler renvoie 500 avec une probabilité `rate` (0.0–1.0).
// Sert à piloter un taux d'erreur pour les alertes burn-rate.
func flakyHandler(w http.ResponseWriter, r *http.Request) {
	rate, _ := strconv.ParseFloat(r.URL.Query().Get("rate"), 64)
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	if cryptoFloat() < rate {
		http.Error(w, "synthetic failure\n", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// panicHandler déclenche un panic. instrument() le récupère et renvoie 500 ;
// le pod ne crash pas. Visible dans Loki et exerce les alertes 5xx.
func panicHandler(_ http.ResponseWriter, _ *http.Request) {
	panic("synthetic panic from /api/panic")
}

// crashHandler tue le process pour démontrer les redémarrages Kubernetes
// (CrashLoopBackOff, readinessProbe, PDB). Réservé au mode chaos.
func crashHandler(w http.ResponseWriter, _ *http.Request) {
	slog.Error("synthetic crash via /api/crash")
	// La réponse n'arrivera probablement pas, mais on essaie quand même.
	_, _ = w.Write([]byte("crashing\n"))
	go func() {
		time.Sleep(50 * time.Millisecond)
		os.Exit(137) // simule un SIGKILL/OOM (exit code = 128 + 9)
	}()
}

// memleakHandler alloue `mb` mégaoctets et les conserve en mémoire vive
// jusqu'au prochain restart (ou /api/memleak/reset).
func memleakHandler(w http.ResponseWriter, r *http.Request) {
	mb := atoiClamp(r.URL.Query().Get("mb"), 1, 256)
	buf := make([]byte, mb*1024*1024)
	// Touche chaque page pour forcer l'allocation physique (sinon, paged-in lazy).
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 0x42
	}
	memHoldMu.Lock()
	memHold = append(memHold, buf)
	total := 0
	for _, b := range memHold {
		total += len(b)
	}
	memHoldMu.Unlock()
	fmt.Fprintf(w, "leaked %d MiB (total held %d MiB)\n", mb, total/1024/1024)
}

func memleakResetHandler(w http.ResponseWriter, _ *http.Request) {
	memHoldMu.Lock()
	memHold = nil
	memHoldMu.Unlock()
	_, _ = w.Write([]byte("memleak buffers released\n"))
}

// chaosGuard renvoie 403 si DEMO_CHAOS_ENABLED n'est pas à 1.
// On wrappe panic/crash/memleak pour éviter qu'ils soient exposés
// sans opt-in en environnement réel.
func chaosGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !chaosEnabled {
			http.Error(w, "chaos endpoints disabled (set DEMO_CHAOS_ENABLED=1)\n", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func atoiClamp(s string, lo, hi int) int {
	n, _ := strconv.Atoi(s)
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// cryptoFloat retourne un float64 uniforme dans [0,1) sans utiliser math/rand
// (évite la gosec G404 et garantit l'indépendance du seed entre instances).
func cryptoFloat() float64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<53))
	if err != nil {
		// fallback déterministe : on tombe sur 0 → jamais d'erreur synthétique
		var b [8]byte
		_, _ = rand.Read(b[:])
		return float64(binary.LittleEndian.Uint64(b[:])>>11) / float64(1<<53)
	}
	return float64(n.Int64()) / float64(1<<53)
}
