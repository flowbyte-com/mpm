# Embedding Benchmark Harness Run Report

## Corpus
- **version**: `v2`
- **documents_count**: `50`
- **queries_count**: `60`
- **positive_queries_count**: `40`
- **negative_queries_count**: `20`
- **scoring_method**: `mpm recall --semantic --json`
- **manifest_path**: `/home/v/src/mpm/.claude/worktrees/issue-15-calibration/bench/corpus/v2/MANIFEST.json`

queries.v1 is a small synthetic labelled set, not a recovery of the historical 30-query benchmark. The historical +0.022 MRR figure attributed to raw-cosine prefixing is NOT reproduced by this harness. The raw-cosine delta in the report is the metric DIFFERENCE between the unprefixed and `search_query:`-prefixed query embeddings on this corpus; a near-zero delta on a synthetic set is expected and does not contradict the historical finding on the lost corpus.

## Regeneration verification (gate 1)
- Status: **OK**
- Expected: `{'model': 'openai-compatible:nomic-embed-text', 'dimension': 768, 'source': 'provider'}`

## Snapshot logical mutation (gate 7, v3 per-query)
- Status: **clean**
- Coverage: memories, reference_chunks, retrieval_metadata, contradiction_queue, challenges (v3)

## Vector participation (C5 / gate 4 v2)
- Positive queries: 40/40 vector-participated
- Negative queries: 20 total — correctly_empty=0, vector_participated=20, fts5_only=0

## Labelled relevance scoring (priority 3)
- top-k: `10` (from corpus.scoring.top_k; v3)
- Positive queries scored: `40` (negative: `20`)
- Mean Precision@k: `0.1075`
- Mean Recall@k:    `0.9875`
- Mean MRR:         `0.9182`
- Mean nDCG@k:      `0.9269`

## Isolation (C4 / gate 7)
- **snapshot_workspace**: `/tmp/mpm-bench/mpm-bench-run-bxlzo6vi`
- **output_dir**: `/tmp/mpm-bench-v2-nomic-a3`
- **snapshot_symlink_rejection**: `passed`
- **output_symlink_rejection**: `passed`
- **env_scrubbing**: `passed (see test_env_scrubbing.py)`
- **binary_provenance**: `{'path': '/home/v/src/mpm/.claude/worktrees/issue-15-calibration/.build/bin/mpm', 'resolved_path': '/home/v/src/mpm/.claude/worktrees/issue-15-calibration/.build/bin/mpm', 'size_bytes': 22601728, 'mtime_iso': '2026-10-19T13:34:57.340063+00:00', 'sha256': '130cb1293d27850d2f30aa8e4fd53cdd4f2f9df74d6d9614c6abeb90c0af4304', 'mpm_version': 'MPM mpm archive/pr29head-17-g90ef900', 'git_head': '90ef90088f154086881d05338a22e2487ad17d5a', 'build_config': 'CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) build -tags fts5 $(BUILD_LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)    ./cmd/mpm', 'canonical': True}`
- **bench_collection**: `bench_v2_corpus`
- **expected_collection_verification**: `bench_v2_corpus`

## Raw-cosine experiment (v3: unprefixed + prefixed)
- Method: independent raw-cosine ranking, NOT hybrid recall
- **Unprefixed** query embedding: Mean Precision@k=`0.1075`, Recall@k=`0.9875`, MRR=`0.9182`, nDCG@k=`0.9269`
- **Prefixed** (`search_query: `) query embedding: Mean Precision@k=`0.1050`, Recall@k=`0.9625`, MRR=`0.9146`, nDCG@k=`0.9184`
- **Delta** (prefixed - unprefixed): ΔPrecision@k=`-0.0025`, ΔRecall@k=`-0.0250`, ΔMRR=`-0.0036`, ΔnDCG@k=`-0.0085`

## Per-query table (hybrid ranker)

| query_id | class | n_rows | sources | vector_participated | search_mode |
|---|---|---|---|---|---|
| `q02001` | positive | 10 | vector | True | hybrid |
| `q02002` | positive | 10 | vector | True | hybrid |
| `q02003` | positive | 10 | vector | True | hybrid |
| `q02004` | positive | 10 | vector | True | hybrid |
| `q02005` | positive | 10 | vector | True | hybrid |
| `q02006` | positive | 10 | vector | True | hybrid |
| `q02007` | positive | 10 | vector | True | hybrid |
| `q02008` | positive | 10 | vector | True | hybrid |
| `q02009` | positive | 10 | vector | True | hybrid |
| `q02010` | positive | 10 | vector | True | hybrid |
| `q02011` | positive | 10 | vector | True | hybrid |
| `q02012` | positive | 10 | vector | True | hybrid |
| `q02013` | positive | 10 | vector | True | hybrid |
| `q02014` | positive | 10 | vector | True | hybrid |
| `q02015` | positive | 10 | vector | True | hybrid |
| `q02016` | positive | 10 | vector | True | hybrid |
| `q02017` | positive | 10 | vector | True | hybrid |
| `q02018` | positive | 10 | vector | True | hybrid |
| `q02019` | positive | 10 | vector | True | hybrid |
| `q02020` | positive | 10 | vector | True | hybrid |
| `q02021` | positive | 10 | vector | True | hybrid |
| `q02022` | positive | 10 | vector | True | hybrid |
| `q02023` | positive | 10 | vector | True | hybrid |
| `q02024` | positive | 10 | vector | True | hybrid |
| `q02025` | positive | 10 | vector | True | hybrid |
| `q02026` | positive | 10 | vector | True | hybrid |
| `q02027` | positive | 10 | vector | True | hybrid |
| `q02028` | positive | 10 | vector | True | hybrid |
| `q02029` | positive | 10 | vector | True | hybrid |
| `q02030` | positive | 10 | vector | True | hybrid |
| `q02031` | positive | 10 | vector | True | hybrid |
| `q02032` | positive | 10 | vector | True | hybrid |
| `q02033` | positive | 10 | vector | True | hybrid |
| `q02034` | positive | 10 | vector | True | hybrid |
| `q02035` | positive | 10 | vector | True | hybrid |
| `q02036` | positive | 10 | vector | True | hybrid |
| `q02037` | positive | 10 | vector | True | hybrid |
| `q02038` | positive | 10 | vector | True | hybrid |
| `q02039` | positive | 10 | vector | True | hybrid |
| `q02040` | positive | 10 | vector | True | hybrid |
| `q03001` | negative | 10 | vector | True | hybrid |
| `q03002` | negative | 10 | vector | True | hybrid |
| `q03003` | negative | 10 | vector | True | hybrid |
| `q03004` | negative | 10 | vector | True | hybrid |
| `q03005` | negative | 10 | vector | True | hybrid |
| `q03006` | negative | 10 | vector | True | hybrid |
| `q03007` | negative | 10 | vector | True | hybrid |
| `q03008` | negative | 10 | vector | True | hybrid |
| `q03009` | negative | 10 | vector | True | hybrid |
| `q03010` | negative | 10 | vector | True | hybrid |
| `q03011` | negative | 10 | vector | True | hybrid |
| `q03012` | negative | 10 | vector | True | hybrid |
| `q03013` | negative | 10 | vector | True | hybrid |
| `q03014` | negative | 10 | vector | True | hybrid |
| `q03015` | negative | 10 | vector | True | hybrid |
| `q03016` | negative | 10 | vector | True | hybrid |
| `q03017` | negative | 10 | vector | True | hybrid |
| `q03018` | negative | 10 | vector | True | hybrid |
| `q03019` | negative | 10 | vector | True | hybrid |
| `q03020` | negative | 10 | vector | True | hybrid |

## Per-query table (raw-cosine experiment — unprefixed)

| query_id | class | n_rows | top_id | top_score |
|---|---|---|---|---|
| `q02001` | positive | 10 | `d02003` | 0.7224565980844685 |
| `q02002` | positive | 10 | `d02001` | 0.7908965612521672 |
| `q02003` | positive | 10 | `d02001` | 0.7685720979951944 |
| `q02004` | positive | 10 | `d02038` | 0.6103388006181981 |
| `q02005` | positive | 10 | `d02038` | 0.8401840875894141 |
| `q02006` | positive | 10 | `d02007` | 0.7511274918211961 |
| `q02007` | positive | 10 | `d02007` | 0.726761881688123 |
| `q02008` | positive | 10 | `d02015` | 0.6429656674112724 |
| `q02009` | positive | 10 | `d02005` | 0.7519106590481728 |
| `q02010` | positive | 10 | `d02006` | 0.730531798142894 |
| `q02011` | positive | 10 | `d02025` | 0.8039011736584162 |
| `q02012` | positive | 10 | `d02025` | 0.7484769006866884 |
| `q02013` | positive | 10 | `d02031` | 0.8428296266582983 |
| `q02014` | positive | 10 | `d02031` | 0.817560760771135 |
| `q02015` | positive | 10 | `d02027` | 0.6765554147670371 |
| `q02016` | positive | 10 | `d02013` | 0.7572010214469018 |
| `q02017` | positive | 10 | `d02009` | 0.7398571979742962 |
| `q02018` | positive | 10 | `d02030` | 0.8193221608329846 |
| `q02019` | positive | 10 | `d02020` | 0.840687972862192 |
| `q02020` | positive | 10 | `d02021` | 0.7786133077175489 |
| `q02021` | positive | 10 | `d02019` | 0.8752155565306289 |
| `q02022` | positive | 10 | `d02016` | 0.8143744754806966 |
| `q02023` | positive | 10 | `d02022` | 0.7648338015569683 |
| `q02024` | positive | 10 | `d02017` | 0.8221392410995436 |
| `q02025` | positive | 10 | `d02004` | 0.7638385825147359 |
| `q02026` | positive | 10 | `d02041` | 0.7068751784569176 |
| `q02027` | positive | 10 | `d02043` | 0.6143070376176738 |
| `q02028` | positive | 10 | `d02043` | 0.6185827254975897 |
| `q02029` | positive | 10 | `d02044` | 0.7394851317224617 |
| `q02030` | positive | 10 | `d02045` | 0.5956950425600767 |
| `q02031` | positive | 10 | `d02046` | 0.8228979273133937 |
| `q02032` | positive | 10 | `d02012` | 0.82417197301448 |
| `q02033` | positive | 10 | `d02047` | 0.9234184450169666 |
| `q02034` | positive | 10 | `d02048` | 0.7992009105679166 |
| `q02035` | positive | 10 | `d02049` | 0.7415151677155529 |
| `q02036` | positive | 10 | `d02050` | 0.6264087744557585 |
| `q02037` | positive | 10 | `d02046` | 0.7721733234251371 |
| `q02038` | positive | 10 | `d02045` | 0.6734695959495999 |
| `q02039` | positive | 10 | `d02009` | 0.5733388880446832 |
| `q02040` | positive | 10 | `d02043` | 0.6820058611819161 |
| `q03001` | negative | 10 | `d02036` | 0.6601850546029657 |
| `q03002` | negative | 10 | `d02007` | 0.6242932419137942 |
| `q03003` | negative | 10 | `d02013` | 0.6368648466753174 |
| `q03004` | negative | 10 | `d02014` | 0.778479470803213 |
| `q03005` | negative | 10 | `d02042` | 0.6162401253970585 |
| `q03006` | negative | 10 | `d02008` | 0.6363622396383314 |
| `q03007` | negative | 10 | `d02007` | 0.6170444450609142 |
| `q03008` | negative | 10 | `d02007` | 0.6429291918308931 |
| `q03009` | negative | 10 | `d02050` | 0.5910471980117904 |
| `q03010` | negative | 10 | `d02007` | 0.6564220520088143 |
| `q03011` | negative | 10 | `d02013` | 0.7229443691498352 |
| `q03012` | negative | 10 | `d02007` | 0.6465189550543173 |
| `q03013` | negative | 10 | `d02007` | 0.6576260417069563 |
| `q03014` | negative | 10 | `d02049` | 0.6092620308166039 |
| `q03015` | negative | 10 | `d02050` | 0.6395925835745226 |
| `q03016` | negative | 10 | `d02007` | 0.6414254444868058 |
| `q03017` | negative | 10 | `d02007` | 0.5696646044948075 |
| `q03018` | negative | 10 | `d02007` | 0.6149838758164705 |
| `q03019` | negative | 10 | `d02049` | 0.6684900593369317 |
| `q03020` | negative | 10 | `d02036` | 0.639531035994016 |

## Per-query table (raw-cosine experiment — `search_query:` prefixed)

| query_id | class | n_rows | top_id | top_score |
|---|---|---|---|---|
| `q02001` | positive | 10 | `d02003` | 0.7433329128055238 |
| `q02002` | positive | 10 | `d02001` | 0.8074466076684451 |
| `q02003` | positive | 10 | `d02001` | 0.785815022830543 |
| `q02004` | positive | 10 | `d02038` | 0.6448339230113498 |
| `q02005` | positive | 10 | `d02038` | 0.8264499507263323 |
| `q02006` | positive | 10 | `d02007` | 0.7441966947191231 |
| `q02007` | positive | 10 | `d02007` | 0.7221606263137256 |
| `q02008` | positive | 10 | `d02015` | 0.6391792208369635 |
| `q02009` | positive | 10 | `d02005` | 0.7697395653833429 |
| `q02010` | positive | 10 | `d02006` | 0.7587105176315067 |
| `q02011` | positive | 10 | `d02025` | 0.8064018985626996 |
| `q02012` | positive | 10 | `d02025` | 0.7563156004070773 |
| `q02013` | positive | 10 | `d02031` | 0.8421711393888579 |
| `q02014` | positive | 10 | `d02031` | 0.8253551389137371 |
| `q02015` | positive | 10 | `d02027` | 0.6982659304139777 |
| `q02016` | positive | 10 | `d02013` | 0.7738184085811204 |
| `q02017` | positive | 10 | `d02009` | 0.7469127510489767 |
| `q02018` | positive | 10 | `d02030` | 0.822397634813297 |
| `q02019` | positive | 10 | `d02020` | 0.8371441538640071 |
| `q02020` | positive | 10 | `d02021` | 0.8054391005714359 |
| `q02021` | positive | 10 | `d02019` | 0.8886294207572435 |
| `q02022` | positive | 10 | `d02016` | 0.8181259397903319 |
| `q02023` | positive | 10 | `d02022` | 0.7750901119717362 |
| `q02024` | positive | 10 | `d02017` | 0.8320365707461804 |
| `q02025` | positive | 10 | `d02004` | 0.7765451502768761 |
| `q02026` | positive | 10 | `d02041` | 0.7155803989107483 |
| `q02027` | positive | 10 | `d02043` | 0.6337822617004302 |
| `q02028` | positive | 10 | `d02043` | 0.6475313079918902 |
| `q02029` | positive | 10 | `d02044` | 0.7720062952250539 |
| `q02030` | positive | 10 | `d02045` | 0.6179217219951461 |
| `q02031` | positive | 10 | `d02046` | 0.8111693306019915 |
| `q02032` | positive | 10 | `d02012` | 0.824994456618265 |
| `q02033` | positive | 10 | `d02047` | 0.9196792071406864 |
| `q02034` | positive | 10 | `d02048` | 0.8193763729212415 |
| `q02035` | positive | 10 | `d02049` | 0.7622104787475218 |
| `q02036` | positive | 10 | `d02050` | 0.6408231547106249 |
| `q02037` | positive | 10 | `d02046` | 0.7526288824496705 |
| `q02038` | positive | 10 | `d02045` | 0.675737337318751 |
| `q02039` | positive | 10 | `d02009` | 0.6130747709479817 |
| `q02040` | positive | 10 | `d02043` | 0.69004365239601 |
| `q03001` | negative | 10 | `d02036` | 0.6891739454796412 |
| `q03002` | negative | 10 | `d02007` | 0.6370704899041091 |
| `q03003` | negative | 10 | `d02013` | 0.64928219982718 |
| `q03004` | negative | 10 | `d02014` | 0.7935503842224929 |
| `q03005` | negative | 10 | `d02049` | 0.6394621213726591 |
| `q03006` | negative | 10 | `d02008` | 0.6575383471524602 |
| `q03007` | negative | 10 | `d02007` | 0.639854492190695 |
| `q03008` | negative | 10 | `d02008` | 0.6131587084399701 |
| `q03009` | negative | 10 | `d02050` | 0.6334510553633891 |
| `q03010` | negative | 10 | `d02007` | 0.6633602256721267 |
| `q03011` | negative | 10 | `d02013` | 0.7399727856770848 |
| `q03012` | negative | 10 | `d02007` | 0.6616489441057161 |
| `q03013` | negative | 10 | `d02050` | 0.6749014036567127 |
| `q03014` | negative | 10 | `d02049` | 0.6136169297562909 |
| `q03015` | negative | 10 | `d02050` | 0.6320342072653288 |
| `q03016` | negative | 10 | `d02007` | 0.6551286201675918 |
| `q03017` | negative | 10 | `d02007` | 0.5900311502192943 |
| `q03018` | negative | 10 | `d02007` | 0.616936822732728 |
| `q03019` | negative | 10 | `d02049` | 0.6755628168820496 |
| `q03020` | negative | 10 | `d02049` | 0.6468707399849221 |

