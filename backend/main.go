package main

import (
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	gsrpc "github.com/centrifuge/go-substrate-rpc-client/v4"
	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/gorilla/mux"
	"golang.org/x/time/rate"
	"github.com/redis/go-redis/v9"
	"context"
)

type RelayRequest struct {
	Dest                 string  `json:"dest"`
	Value                string  `json:"value"`
	GasLimit             string  `json:"gasLimit"`
	StorageDepositLimit  *string `json:"storageDepositLimit"`
	DataHex              string  `json:"dataHex"`
	Signer               string  `json:"signer"`
}

type RelayResponse struct {
	Status string `json:"status"`
	TxHash string `json:"txHash,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Limiter struct {
	mu       sync.Mutex
	visitors map[string]*rate.Limiter
}

func NewLimiter() *Limiter {
	return &Limiter{visitors: make(map[string]*rate.Limiter)}
}

func (l *Limiter) get(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.visitors[ip]; ok {
		return lim
	}
	lim := rate.NewLimiter(rate.Every(2*time.Second), 3)
	l.visitors[ip] = lim
	return lim
}

func main() {
	endpoint := os.Getenv("WS_ENDPOINT")
	seed := os.Getenv("RELAYER_SEED")
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	if endpoint == "" {
		endpoint = "wss://ws.azero.dev"
	}
	if seed == "" {
		log.Fatal("RELAYER_SEED is required")
	}

	api, err := gsrpc.NewSubstrateAPI(endpoint)
	if err != nil {
		log.Fatalf("connect error: %v", err)
	}
	meta, err := api.RPC.State.GetMetadataLatest()
	if err != nil {
		log.Fatalf("metadata error: %v", err)
	}

	kp, err := signature.KeyringPairFromSecret(seed, 0)
	if err != nil {
		log.Fatalf("key error: %v", err)
	}

	limiter := NewLimiter()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx := context.Background()

	router := mux.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	router.HandleFunc("/relay/mint", func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("X-Forwarded-For")
		if ip == "" {
			ip = r.RemoteAddr
		}
		// Check ban
		banned, _ := rdb.Exists(ctx, "ban:"+ip).Result()
		if banned > 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "ip banned"})
			return
		}
		if !limiter.get(ip).Allow() {
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "rate limited"})
			return
		}

		var req RelayRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "invalid json"})
			return
		}

		// Prepare params
		dest, err := types.NewAddressFromSS58(req.Dest)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "invalid dest"})
			return
		}
		value := types.NewU128(0)
		gas := types.NewU64(0)
		if len(req.GasLimit) > 0 {
			// ignore parse errors, default 0 lets chain compute
		}
		var stor types.OptionU128
		stor = types.NewOptionU128(types.U128{Int: *types.NewU128(0).Int})
		data, err := hex.DecodeString(req.DataHex[2:])
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "invalid data hex"})
			return
		}

		call, err := types.NewCall(meta, "Contracts.call", dest, value, gas, stor, types.NewBytes(data))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "call build error"})
			return
		}

		ext := types.NewExtrinsic(call)
		// Get nonce
		key, err := types.NewAddressFromAccountID(kp.PublicKey)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "key error"})
			return
		}
		kr, err := api.RPC.System.AccountNextIndex(key)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "nonce error"})
			return
		}
		genesisHash, err := api.RPC.Chain.GetBlockHash(0)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "genesis error"})
			return
		}
		rv, err := api.RPC.State.GetRuntimeVersionLatest()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "runtime error"})
			return
		}

		o := types.SignatureOptions{
			BlockHash:          genesisHash,
			Era:                types.ExtrinsicEra{IsImmortalEra: true},
			GenesisHash:        genesisHash,
			Nonce:              types.NewU32(uint32(kr)),
			SpecVersion:        rv.SpecVersion,
			TransactionVersion: rv.TransactionVersion,
			Tip:                types.NewU128(0),
		}
		if err := ext.Sign(kp, o); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "sign error"})
			return
		}
		sub, err := api.RPC.Author.SubmitAndWatchExtrinsic(ext)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(RelayResponse{Status: "error", Error: "submit error"})
			return
		}
		defer sub.Unsubscribe()
		var finalHash types.Hash
		for {
			status := <-sub.Chan()
			if status.IsInBlock {
				finalHash = status.AsInBlock
			}
			if status.IsFinalized {
				finalHash = status.AsFinalized
				break
			}
		}
		// Read events at finalized block
		keyEvents, err := types.CreateStorageKey(meta, "System", "Events", nil)
		if err == nil {
			raw, err2 := api.RPC.State.GetStorageRaw(keyEvents, finalHash)
			if err2 == nil && raw != nil {
				events := types.EventRecords{}
				if err3 := types.DecodeFromBytes(*raw, &events); err3 == nil {
					// Failure tracking
					if len(events.System_ExtrinsicFailed) > 0 {
						// Increment fail counter and possibly ban
						cnt, _ := rdb.Incr(ctx, "fail:"+ip).Result()
						rdb.Expire(ctx, "fail:"+ip, time.Hour)
						if cnt >= 3 {
							rdb.Set(ctx, "ban:"+ip, "1", time.Hour)
						}
					} else {
						// Success: reset fail counter and log
						rdb.Del(ctx, "fail:"+ip)
						// Append log entry
						logEntry := map[string]any{
							"timestamp": time.Now().Unix(),
							"tx_hash":   finalHash.Hex(),
							"book_id":   r.URL.Query().Get("book_id"),
						}
						b, _ := json.Marshal(logEntry)
						rdb.LPush(ctx, "mint:logs", b)
						rdb.LTrim(ctx, "mint:logs", 0, 999)
					}
				}
			}
		}
		json.NewEncoder(w).Encode(RelayResponse{Status: "submitted", TxHash: finalHash.Hex()})
	}).Methods("POST")

	// Metrics endpoint for frontend
	router.HandleFunc("/metrics/mint", func(w http.ResponseWriter, r *http.Request) {
		limit := int64(50)
		vals, err := rdb.LRange(ctx, "mint:logs", 0, limit-1).Result()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`[]`))
			return
		}
		var out []map[string]any
		for _, v := range vals {
			var m map[string]any
			if json.Unmarshal([]byte(v), &m) == nil {
				out = append(out, m)
			}
		}
		json.NewEncoder(w).Encode(out)
	}).Methods("GET")

	addr := ":8080"
	log.Printf("Relay server listening on %s", addr)
	http.ListenAndServe(addr, router)
}
