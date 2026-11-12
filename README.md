# gar (Go And Run)

O agente registra o comando, o humano roda no terminal dele e o agente lê o resultado.
Serve para quando o agente não pode rodar sozinho: o guard bloqueia `ssh`, ou o comando pede senha.

## Fluxo

1. O agente registra os passos:
   ```sh
   gar add --why "atualizar o macbot" -- \
     'ssh macbot "cd ~/Sites/dotfiles-2026 && git pull --ff-only origin main && ./switch"' \
     'bora --version'
   # caso 1020 registrado
   # [HUMAN] ! gar run 1020
   ```
2. O agente deixa `gar wait 1020` rodando em background.
3. O humano roda `! gar run 1020`, confere os passos e responde `y`. Cada passo roda num PTY
   (`$SHELL -c`), então prompt de `sudo`/ssh e Ctrl-C funcionam. Para no primeiro que falhar.
4. O `gar wait` do agente sai com o registro em JSON: saída e exit code de cada passo.

## Comandos

| Comando | O que faz |
|---|---|
| `gar add [--why s] [--expect s] [--from s] [--json] -- <cmd>...` | Registra um caso; cada argumento depois de `--` é um passo. `--from` padrão: `$GAR_FROM`. |
| `gar run <id> [--yes] [--again]` | Só humano: exige TTY em stdin e stdout. Mostra os passos, pergunta `[y/N]` (`--yes` pula). `--again` roda de novo um caso terminado. Responder não cancela o caso. |
| `gar wait <id> [--timeout dur]` | Espera o caso terminar (poll de 500 ms) e imprime o JSON. Sem timeout padrão. |
| `gar show <id> [--json]` | Resumo: passo, exit, duração, últimas linhas da saída. |
| `gar ls [--status s] [--limit n]` | Lista os casos, mais novos primeiro (padrão 20). |
| `gar cancel <id>` | Cancela um caso `pending`. |
| `gar sql "<query>"` | Opcional: precisa do `duckdb` no PATH. Views `runs` e `steps` (um passo por linha, com `run_id`). |

## Exit codes

| Código | Quando |
|---|---|
| 0 | ok; `wait` com caso `done` |
| 1 | `wait` com caso `failed`; erro interno |
| 2 | `wait` com caso `cancelled`; `run` respondido com não |
| 3 | `sql` sem `duckdb` no PATH |
| 4 | caso não existe, ou o status não permite (ex.: `run` em caso já terminado sem `--again`, `cancel` fora de `pending`) |
| 5 | `run` sem TTY em stdin ou stdout |
| 64 | uso errado |
| 124 | `wait` estourou o `--timeout` |
| 128+n | `run` interrompido pelo sinal n (SIGINT → 130, SIGTERM → 143): caso fica `cancelled` |

O `run` sai com o exit do último passo que rodou.

## Estado

`$GAR_HOME`, padrão `~/.local/state/gar/`:

- `runs/<id>.json` — um arquivo por caso, fonte da verdade, gravado de forma atômica (temp + rename)
  a cada passo, então o `wait` vê o progresso.
- `counter` — último ID, protegido por `flock`. IDs começam em 1001.
- `lock` — serializa mudanças de status entre processos (`run` × `cancel`).

Status do caso: `pending → running → done | failed | cancelled` (ou `pending → cancelled`).
Status do passo: `pending`, `running`, `done`, `failed`, `cancelled`, `skipped`.
`output` guarda a saída combinada do passo sem ANSI, com `\r\n` → `\n`, só os últimos 64 KiB
(`output_truncated: true` quando cortou).

## Build

```sh
CGO_ENABLED=0 go build -o gar .
go vet ./... && go test ./...
```
