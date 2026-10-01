# Antigravity Account Switcher

- **No dashboard:** o diálogo **Add account** (modo AliasMode profile) lista esses perfis com a conta de cada
  um e deixa entrar numa conta por um perfil livre: o proxy vem então do perfil (cruzado com o Proxy Pool pelo
  nome `proxy-<host>-<porta>`), então o login e a troca do código saem do mesmo IP. Uma conta por perfil;
  nada vem pré-selecionado.
[English](README.md) | [Português (Brasil)](README.pt-BR.md)

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)](https://go.dev/)
[![CI Status](https://github.com/AugusttoDaniel/antigravity-account-switcher/actions/workflows/ci.yml/badge.svg)](https://github.com/AugusttoDaniel/antigravity-account-switcher/actions)
[![CodeRabbit Pull Request Reviews](https://img.shields.io/coderabbit/prs/github/AugusttoDaniel/antigravity-account-switcher?utm_source=oss&utm_medium=github&utm_campaign=AugusttoDaniel%2Fantigravity-account-switcher&labelColor=171717&color=FF570A&link=https%3A%2F%2Fcoderabbit.ai&label=CodeRabbit+Reviews)](https://coderabbit.ai)

Gerenciamento automático de pool de contas, monitoramento de cotas em tempo real e failover transparente em erros HTTP 429 para o **Google Antigravity 2.0** e CLI (`agy`).

> [!WARNING]
> **Aviso — uso apenas para interoperabilidade e fins educacionais.**
> Este é um projeto independente e não oficial. **Não é afiliado, autorizado ou endossado** pelo Google, ADS Power, AliasMode, OmniRoute ou qualquer terceiro; todas as marcas pertencem aos seus respectivos donos.
> A ferramenta interopera com serviços de terceiros que possuem seus próprios Termos de Serviço. Automatizar login, agrupar/rotacionar várias contas para contornar limites de requisição e usar perfis de navegador antidetect **pode violar esses Termos** e resultar em suspensão de contas ou outras consequências. **Você é o único responsável** por garantir que seu uso esteja em conformidade com todos os termos, leis e regulamentos aplicáveis. O software é fornecido "COMO ESTÁ", sem garantias e sem responsabilidade dos autores — veja [LICENSE](LICENSE).

---

## O que é o Antigravity 2.0 e por que esta ferramenta é necessária?

O **Google Antigravity 2.0** é o novo aplicativo desktop de desenvolvimento com IA criado pela equipe do Google DeepMind (completamente independente e diferente das antigas extensões de preview para VS Code).

Ao trabalhar intensamente com o Antigravity 2.0, é muito comum atingir os limites de requisições (`HTTP 429 RESOURCE_EXHAUSTED` ou a janela deslizante de 5 horas de cota) nos modelos Claude, GPT e Gemini.

O **Antigravity Account Switcher** roda como um supervisor local transparente e de alta performance que intercepta as requisições do Antigravity 2.0. Quando a conta ativa esgota sua cota, o switcher **alterna instantaneamente para a próxima conta disponível no seu pool e repete a requisição em memória** — sem interromper o raciocínio do agente, sem quebrar os streams de resposta e sem gerar erros na interface.

---

## Instalando o Google Antigravity 2.0 no Linux

> [!IMPORTANT]
> **O Google Antigravity 2.0 NÃO é distribuído via gerenciadores de pacotes (`apt`, `snap`, `dnf`, `pacman` ou `flatpak`).**  
> O Google disponibiliza a aplicação diretamente como um arquivo `.tar.gz` contendo o binário executável e as bibliotecas necessárias.

No Linux, programas distribuídos dessa forma devem ser instalados ou na **Pasta de Usuário XDG** (recomendado, não exige root nem `sudo`) ou na **Pasta do Sistema `/opt`** (requer privilégios de administrador).

### Opção 1: Instalação de Usuário (Recomendada — Sem necessidade de `sudo`)

Essa opção segue a especificação **Linux XDG Base Directory** (`~/.local/share/` e `~/.local/bin/`). Ela não afeta outros usuários da máquina nem arquivos do sistema operacional:

```bash
# 1. Cria a pasta da aplicação
mkdir -p ~/.local/share/antigravity

# 2. Descompacta o arquivo baixado do Google
tar -xzf ~/Downloads/Antigravity-linux-x64.tar.gz -C ~/.local/share/antigravity --strip-components=1

# 3. Garante permissão de execução no binário
chmod +x ~/.local/share/antigravity/antigravity

# 4. Cria o atalho em ~/.local/bin (já presente no PATH das principais distribuições Linux)
mkdir -p ~/.local/bin
ln -sf ~/.local/share/antigravity/antigravity ~/.local/bin/antigravity
```

### Opção 2: Instalação no Sistema (Requer `sudo` / Padrão FHS)

Segue o padrão **Filesystem Hierarchy Standard (FHS)** para softwares de terceiros em `/opt`:

```bash
# 1. Cria a pasta no sistema
sudo mkdir -p /opt/antigravity

# 2. Descompacta o arquivo
sudo tar -xzf ~/Downloads/Antigravity-linux-x64.tar.gz -C /opt/antigravity --strip-components=1

# 3. Cria o link simbólico global
sudo ln -sf /opt/antigravity/antigravity /usr/local/bin/antigravity
```

---

## Como o Switcher se Conecta ao Antigravity 2.0

### Detecção Automática (Zero Configuração)
Se você seguiu qualquer um dos métodos de instalação recomendados acima, **você não precisa configurar nenhum caminho manualmente**.  
Ao executar `antigravity-account-switcher launch`, o switcher busca automaticamente o executável nos caminhos padrão do Linux:
1. `~/.local/bin/antigravity`
2. `~/.local/share/antigravity/antigravity`
3. `/usr/local/bin/antigravity`
4. `/opt/antigravity/antigravity`
5. `~/tools/Antigravity/Antigravity-x64/antigravity` *(fallback para pastas personalizadas)*
6. Qualquer comando `antigravity` ou `agy` presente no `$PATH` do seu sistema

### Configuração de Caminho Personalizado
Caso tenha descompactado o Antigravity 2.0 em uma pasta diferente, você pode especificar o caminho facilmente por qualquer um destes métodos:

**Método 1: Configuração permanente via CLI (Recomendado)**
```bash
antigravity-account-switcher config set antigravity_bin /caminho/para/seu/antigravity
```

**Método 2: Variável de Ambiente**
```bash
export ANTIGRAVITY_BIN="/caminho/para/seu/antigravity"
```

**Método 3: Flag no momento da execução**
```bash
antigravity-account-switcher launch --bin /caminho/para/seu/antigravity
```

---

## Início Rápido (3 Passos)

### 1. Compilar e Instalar o Switcher
```bash
git clone https://github.com/AugusttoDaniel/antigravity-account-switcher.git
cd antigravity-account-switcher
make install
```
*Compila um único binário estático (sem dependências CGO) e o instala em `~/.local/bin/antigravity-account-switcher`.*

### 2. Cadastrar suas Contas
Autentique uma ou mais contas do Google com fluxo OAuth seguro:
```bash
antigravity-account-switcher add-account
```
*(Se você já possui o Antigravity 2.0 instalado e com login ativo, o switcher importa automaticamente sua conta no primeiro uso!)*

### 3. Iniciar o Antigravity 2.0
```bash
antigravity-account-switcher launch
```
O switcher inicia o proxy em segundo plano, abre o Antigravity 2.0 com variáveis de ambiente isoladas e monitora a saúde das contas. Ao fechar o Antigravity 2.0, o switcher é encerrado automaticamente no mesmo instante.

---

## Integração com o Desktop (GNOME / KDE / XFCE)

Para abrir o Antigravity 2.0 diretamente do menu de aplicativos ou da barra de tarefas/dock do seu Linux:

```bash
antigravity-account-switcher install-desktop
```
Esse comando automaticamente:
- Localiza o binário do Antigravity 2.0 na sua máquina.
- Extrai e configura o ícone oficial da aplicação em `~/.local/share/icons/antigravity.png`.
- Cria o arquivo de atalho `~/.local/share/applications/antigravity.desktop` apontando para `antigravity-account-switcher launch %F`.

Para remover o atalho desktop a qualquer momento:
```bash
antigravity-account-switcher uninstall-desktop
```

---

## Referência de Comandos da CLI

A CLI disponibiliza comandos para supervisão, troca manual e configuração:

| Comando | Descrição |
| :--- | :--- |
| `launch` | **(Recomendado)** Inicia o Antigravity 2.0 sob a supervisão do proxy acoplado. |
| `serve` | Inicia o proxy local, monitor de cotas em segundo plano e dashboard web como serviço. |
| `wrap -- <comando>` | Executa qualquer comando arbitrário injetando o proxy apenas naquele processo. |
| `add-account` | Inicia o fluxo OAuth2 loopback (RFC 8252) no navegador para cadastrar uma nova conta Google. |
| `add-account-adspower` | Cadastra uma conta através de um perfil isolado do ADS Power + proxy (login assistido). |
| `import-adspower` | Cadastra em lote todos os perfis do ADS Power como contas, reusando o proxy de cada perfil. |
| `export-omniroute` | Exporta as contas para o OmniRoute (arquivos de token agy e/ou a API dele), amarrando o proxy de cada conta. |
| `set-account-proxy` | Define ou atualiza a URL de proxy de saída de uma conta específica. |
| `list-accounts` | Exibe todas as contas cadastradas, qual está ativa e as porcentagens de cota. |
| `refresh-quotas` | Força a sincronização imediata de cotas com o Google para todas as contas. |
| `status` | Mostra a conta ativa no momento, métricas de tokens e saúde do switcher. |
| `config` | Consulta ou atualiza configurações persistentes (`get`, `set`, `list`). |
| `install-desktop` | Cria o atalho `.desktop` no menu do GNOME/XDG com o ícone oficial. |
| `uninstall-desktop`| Remove o atalho `.desktop` do menu do sistema. |
| `version` | Exibe a versão compilada, hash do commit e data de build. |

### Flags Úteis

- **Abrir o Dashboard Web no Navegador ao Iniciar:**
  ```bash
  antigravity-account-switcher launch --open
  ```
- **Adicionar Conta em Servidor Remoto / Sem Navegador (SSH / Headless):**
  ```bash
  antigravity-account-switcher add-account --no-browser
  ```
- **Adicionar Conta Sem Vazar o Seu IP:** o botão **Authenticate New Google Account** do dashboard
  pede primeiro o proxy da conta (do pool, ou digitado), faz a troca de tokens por ele e salva o
  proxy na conta. Depois ele:
  - **abre um perfil isolado do AliasMode / ADS Power** (um clique): o perfil é criado já ligado a
    esse proxy, o navegador dele abre o Google e você faz o login dentro dessa janela. O dashboard
    acha o AliasMode na porta padrão (`http://127.0.0.1:50400`) ou a do ADS Power; para apontar
    outro endereço use `config set adspower_api_url <url>` (precisa ser nesta máquina, pois as
    credenciais do proxy são enviadas a ele), e `adspower_api_key` e `adspower_engine` (padrão
    `cloak`, Chromium) se precisar; ou
  - **mostra o link de login** em vez de abrir o seu navegador padrão (esse navegador chega ao
    Google pelo seu IP real), para você abrir num perfil de navegador que use o mesmo proxy.

  Pelo terminal, `add-account --proxy <url>` faz o mesmo para a troca de tokens, e
  `add-account-adspower` executa o fluxo com perfil.
- **Especificar Porta Customizada:**
  ```bash
  antigravity-account-switcher launch --port 1831
  ```
- **Fallback Multi-Modelo no Momento da Execução:**
  ```bash
  antigravity-account-switcher launch --fallback-secondary --model-primary gemini-2.5-pro --model-secondary claude-3-7-sonnet
  ```
- **Modo Privacidade no Dashboard Web:**
  Clique no botão **Privacidade** no cabeçalho do painel ou pressione <kbd>P</kbd> para borrar visualmente e ofuscar todos os e-mails das contas Google (cards de contas, rota ativa e logs de eventos em tempo real), permitindo capturas de tela e transmissões sem vazamento de dados pessoais.

### Onboarding Isolado via ADS Power

Para manter o login e o tráfego de cada conta longe do seu IP e fingerprint reais, cadastre as
contas através de perfis antidetect do [ADS Power](https://www.adspower.com/). Toda chamada OAuth —
consent, troca de code, userinfo e refresh de token em segundo plano — sai pelo proxy da conta, então
o Google nunca vê a sua conexão real.

Pré-requisito: ADS Power rodando localmente com a Local API habilitada (padrão
`http://local.adspower.net:50325`).

1. Defina um pool estático de proxies para contas novas no arquivo de config:
   ```json
   { "proxies": ["http://user:pass@host-a:8080", "socks5://user:pass@host-b:1080"] }
   ```
2. Cadastre uma conta (cria um perfil isolado amarrado a um proxy nunca usado e abre o navegador do
   perfil na tela de consentimento do Google para você fazer login):
   ```bash
   antigravity-account-switcher add-account-adspower --email voce@gmail.com
   ```
   - Conta já existente reusa o proxy/perfil dela; conta nova puxa um proxy nunca usado do pool. Use
     `--proxy <url>` para forçar um proxy específico.
3. Ou cadastre em lote todos os perfis existentes do ADS Power, reusando o proxy de cada perfil:
   ```bash
   antigravity-account-switcher import-adspower --all
   ```

> O login é **assistido**: o switcher abre o perfil isolado na tela de consentimento e você conclui
> o login do Google (incluindo 2FA) dentro daquela janela. O switcher nunca armazena credenciais.

### Levando os proxies do OmniRoute pro AliasMode

```bash
antigravity-account-switcher sync-proxies-to-aliasmode --dry-run   # mostra o que seria criado
antigravity-account-switcher sync-proxies-to-aliasmode             # cria os perfis
```

Cria um perfil vazio do AliasMode por proxy do registro do OmniRoute (`proxy-<host>-<porta>`), já
ligado a esse proxy, pronto pra ser escolhido ao adicionar uma conta. A Local API não tem endpoint de
registro de proxies, então um perfil é a única forma de guardar um proxy lá.

- **Credenciais:** o OmniRoute nunca lista a senha de um proxy, só a devolve pra um proxy atribuído a
  uma conexão. Esses vêm do OmniRoute. Os demais são cruzados, por `host:porta`, com o pool de proxies
  local (cole a tabela do provedor no **Proxy Pool** do dashboard, ou use o `proxies` da config);
  proxies sem credencial em lugar nenhum são reportados e pulados. As senhas nunca são impressas.
- **Pode repetir:** um proxy cujo perfil já existe com esse nome é deixado quieto, então uma segunda
  rodada só acrescenta o que faltava. (A Local API não informa o proxy de um perfil, então um perfil
  feito à mão com o mesmo proxy sob outro nome não é reconhecido.)
- **Só local:** a API de perfis precisa estar nesta máquina, já que as credenciais são enviadas a ela.
  Confira o resultado na coluna **Proxy** do AliasMode e prove o isolamento (o IP do perfil tem que ser
  o do proxy) antes de entrar em qualquer conta.

---

### Exportando contas para o OmniRoute

O comando `export-omniroute` envia as contas gerenciadas aqui para o
[OmniRoute](https://github.com/diegosouzapw/OmniRoute) como conexões `agy`, dando refresh em cada
conta pelo proxy dela antes. Pode gerar arquivos de token por conta para import manual:

```bash
antigravity-account-switcher export-omniroute --out ./omniroute-tokens
```

Também pode enviar direto para uma instância local do OmniRoute e amarrar o proxy de cada conta lá.
Rode `antigravity-account-switcher export-omniroute --help` para as flags do modo API.

Contas que o OmniRoute já tem são puladas, nunca duplicadas: uma já importada (`agy`) fica como
está, a menos que você passe `--overwrite` para atualizar os tokens dela lá, e uma conectada pelo
login de Antigravity do próprio OmniRoute é pulada, a menos que você passe `--allow-duplicate`,
já que importá-la faria o OmniRoute usar a mesma conta Google duas vezes.

O proxy da conta acompanha ela até o OmniRoute (`--assign-proxies`, ligado por padrão), **inclusive
para contas que o OmniRoute já tem**: o proxy é cadastrado lá e vinculado à conexão da conta, e em
seguida o OmniRoute é consultado sobre qual proxy a conexão passou a usar; o comando falha com
mensagem clara se não for o vinculado. Só escreve quando os dois diferem, então rodar de novo não
muda nada, e nunca remove um proxy: conta sem proxy é deixada como está. A seção **OmniRoute Sync**
do dashboard mostra a mesma comparação e tem um botão **Bind in OmniRoute** (com clique de
confirmação) para as contas cujo proxy é diferente ou está ausente lá.


**Importa qual cliente do Google fez o login da conta.** O Google amarra um refresh token ao cliente OAuth
que o emitiu, e o OmniRoute renova todo token do Antigravity (os providers `agy` e `antigravity` do mesmo
jeito) com o cliente dele (`1071006060591-…`). O Antigravity 2.0 instalado traz mais de um cliente, e o
switcher usava o primeiro por padrão (`884354919052-…`). Um token emitido pelo cliente do switcher é aceito
na importação, funciona por cerca de uma hora (a vida do access token) e depois a renovação do OmniRoute
falha para sempre com `unauthorized_client`: a conexão parece saudável, depois fica inativa, com o
disjuntor de renovação aberto e sem modelos sincronizados. Trocar `agy` por `antigravity` não resolve, já
que os dois renovam do mesmo jeito.

- Cada conta agora lembra qual cliente emitiu o token dela e renova com ele; uma conta de antes disso
  descobre no primeiro refresh. Fixar um cliente só o escolhe para logins **novos**.
- `config set oauth_client_id 1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com`
  faz os logins novos usarem o cliente do OmniRoute (`ANTIGRAVITY_CLIENT_ID` faz o mesmo e vence). Uma
  conta já logada com o outro cliente precisa entrar de novo; a conexão antiga no OmniRoute é então
  trocada com `export-omniroute --overwrite`.
- `export-omniroute --api` agora confere cada conta antes de enviar: pergunta ao Google, pelo proxy da
  própria conta (nunca pelo seu IP real), se o cliente do OmniRoute consegue renovar o token, e não exporta
  o que ele não conseguiria, dizendo por quê. `--skip-client-check` exporta mesmo assim;
  `--omniroute-client-id` serve para um OmniRoute cujo operador definiu `ANTIGRAVITY_OAUTH_CLIENT_ID`.
---

### Contas OpenAI Codex

O mesmo isolamento vale para logins do ChatGPT/Codex. As contas Codex ficam numa tabela própria e
nunca entram no pool de roteamento do Antigravity, no poller de cotas nem na exportação pro OmniRoute.

```bash
# Login por um proxy dedicado, dentro de um perfil isolado do AliasMode/ADS Power (recomendado)
antigravity-account-switcher codex-add --adspower --proxy "http://usuario:senha@host:porta"
# ou imprime a URL de login e você a abre num navegador que já usa esse proxy
antigravity-account-switcher codex-add --proxy "http://usuario:senha@host:porta"

antigravity-account-switcher codex-import          # adota o ~/.codex/auth.json atual do Codex CLI
antigravity-account-switcher codex-list
antigravity-account-switcher codex-switch eu@example.com   # escreve ~/.codex/auth.json (respeita $CODEX_HOME)
antigravity-account-switcher codex-refresh --all           # renova os tokens pelo proxy de cada conta
antigravity-account-switcher codex-set-proxy eu@example.com "http://usuario:senha@host:porta"
```

- `codex-add` se recusa a rodar sem `--proxy` (passe `--allow-direct` para aceitar o seu IP real).
  Por padrão não abre navegador: o navegador padrão chegaria na OpenAI pelo seu IP real. O
  callback do OAuth usa a porta `1455`, o único redirect registrado no cliente público do Codex
  CLI, então feche qualquer `codex login` em andamento antes.
- `codex-switch` não faz chamada de rede. Antes de sobrescrever o `auth.json`, ele salva a rotação
  de token que o Codex CLI fez na conta que está saindo; sem isso, voltar a ela usaria um refresh
  token que o emissor já aposentou. Feche as sessões do Codex antes de trocar.
- `codex-refresh` passa pelo proxy da própria conta e falha fechado se ela não tiver proxy ou se
  ele estiver inutilizável. Um `invalid_grant` marca a conta como `error`: entre de novo com `codex-add`.
- Um e-mail compartilhado por um login pessoal e um de workspace é ambíguo: passe o id da conta.
- **Limites:** `codex-usage [--all] [--cached] [conta]` mostra as janelas de 5 horas e semanal de
  cada conta, os horários de reset, créditos e se o limite foi atingido. Lê o mesmo endpoint que o
  Codex CLI usa (`/backend-api/wham/usage`) pelo proxy da própria conta e falha fechado sem proxy;
  um token de acesso expirado é renovado uma vez e a leitura é repetida. `--cached` mostra o último
  snapshot guardado, sem rede. O dashboard mostra o mesmo em barras na coluna **Limits**, com **Usage**
  por conta e **Refresh usage** para todas (lidas uma a uma, nunca em rajada). Nada consulta por
  temporizador: os limites só são lidos quando você pede.
- **Entrega ao OmniRoute:** `codex-export-omniroute` importa contas Codex no OmniRoute
  (`/api/providers/codex-auth/import-bulk`) e liga o proxy de cada conta à sua conexão. Sem `--yes` só
  imprime o plano. **O refresh token do Codex é de uso único**, então os tokens só podem viver num lugar:
  depois que o OmniRoute os tem, ele os renova, e este switcher marca a conta como *in OmniRoute* e se
  recusa a renovar, ler limites ou trocar o Codex CLI pra ela (`--force` ignora isso, ao custo de quebrar
  a sessão do OmniRoute nessa conta). Nada é renovado antes da exportação, pois isso giraria o token à
  toa. Uma conta que o OmniRoute já tem por login próprio é deixada quieta (nenhum token enviado, sem
  marca), uma que precisa de novo login é pulada, e um novo login com `codex-add` toma a conta de volta
  (é uma família de tokens nova). `--overwrite` troca uma conexão que o OmniRoute já tem, mas nunca
  para uma conta já entregue: o que este switcher guarda está desatualizado a essa altura.
- **Dashboard:** o painel **OpenAI Codex Accounts** lista as contas (proxies mascarados, tokens nunca
  enviados à página) e oferece Use, Refresh, Proxy, Remove e **Add Codex account**, pelo mesmo pool de
  proxies e perfil do AliasMode das contas Google.
- **Porta do callback bloqueada (comum no Windows):** o Windows pode reservar `1374-1473` para
  Hyper-V, WSL ou Docker (`netsh int ipv4 show excludedportrange protocol=tcp`), faixa que contém a
  `1455`, então nenhum programa consegue escutar nela. O switcher detecta isso e passa para um passo
  manual: depois do login, o navegador cai numa página que não carrega; copie o endereço completo e
  cole no dashboard (ou no terminal, no `codex-add`). O endereço carrega um código de uso único, então
  não o compartilhe.
- **Aquecimento:** a janela de limite de uma conta Codex começa com o primeiro pedido dela.
  `codex-warmup [conta]` envia um pedido mínimo pelo proxy da própria conta agora, e
  `codex-warmup-schedule <conta> --times "07:00,12:30"` faz isso sozinho nesses horários locais (`--off`
  desliga, sem flags mostra). O agendamento roda **dentro do `serve`**, então nada acontece com o switcher
  parado. Vem desligado em toda conta até você ligar. Gasta um pouco de cota a cada vez, e cada pedido sai
  pelo IP do proxy da conta, então é conservador de propósito: no máximo 6 vezes por dia, cada horário roda
  uma vez (sem nova tentativa após falha), as contas rodam uma de cada vez com pausa entre elas, cada horário
  começa com um deslocamento estável por conta de até 90 segundos, e um horário que o switcher perdeu por
  mais de 10 minutos é registrado como perdido em vez de disparar atrasado. O modelo não é fixo: é o que a
  lista de modelos da própria conta põe em primeiro (`--model` troca). Contas sem proxy, entregues ao
  OmniRoute ou que precisam de novo login são puladas, nunca enviadas. O dashboard tem **Warm** (pede um
  segundo clique) e **Schedule** por conta, e mostra os horários e o último resultado.
- As constantes do protocolo (emissor, client id público, escopos, formato do `auth.json`) vêm do
  repositório Apache-2.0 [openai/codex](https://github.com/openai/codex). Usar várias contas está
  sujeito aos termos da OpenAI; cumpri-los é responsabilidade sua.

---


## Fallback Multi-Modelo & Auto-Recuperação

O switcher conta com um sistema inteligente de contingência multi-modelo para evitar interrupções no fluxo de desenvolvimento:

### 1. Fallback Intra-Conta de Modelos
Quando a cota do modelo principal se esgota (ex.: `gemini-2.5-pro` ou `claude-3-7-sonnet`), o switcher pode recorrer automaticamente a um modelo secundário mais leve (como `gemini-2.5-flash` ou `claude-3-5-sonnet`) na **mesma conta** antes de alternar para a próxima conta do pool.
- Suporta tanto fallback **entre famílias** (`claude-3-7-sonnet` -> `gemini-2.5-flash`) quanto **dentro da mesma família** (`gemini-2.5-pro` -> `gemini-2.5-flash`, que possuem limites de cota independentes no Google Cloud Code PA).
- Conta com **otimização preditiva**: se a telemetria indicar 0% de cota restante no modelo primário, as requisições são redirecionadas proativamente sem a penalidade de latência de esperar por um erro HTTP 429.
- Assim que o monitor em segundo plano detecta a restauração da cota primária, as requisições voltam automaticamente ao modelo principal configurado.

### 2. Configuração pelo Dashboard Web
Você pode gerenciar as preferências de modelos diretamente pelo navegador (`http://127.0.0.1:8080` ou `launch --open`):
- **Toggle em Tempo Real**: Ative ou desative o fallback intra-conta instantaneamente.
- **Descoberta de Modelos**: Clique em **Buscar Modelos** para inspecionar os endpoints do `language_server` / Cloud Code PA e listar todos os modelos disponíveis.
- **Recarregamento Sem Reiniciar**: Ao salvar as configurações, o `config.json` é atualizado e o motor de proxy aplica as mudanças imediatamente sem necessidade de reiniciar o Antigravity 2.0 ou o daemon.

### 3. Recuperação Automática de Assinatura de Pensamento (HTTP 400)
Ao alternar entre provedores de modelos (ex.: Claude <-> Gemini) ou em sessões de múltiplos turnos, o Google Cloud Code PA valida blocos criptográficos HMAC de raciocínio (`thought_signature` ou blocos de `thinking` do Claude). Assinaturas incompatíveis ou corrompidas causam erro `HTTP 400 ("Corrupted thought signature" ou "Invalid signature in thinking block")`.

**O switcher recupera esse erro automaticamente em tempo real:**
- O proxy intercepta a resposta HTTP 400 antes que ela chegue ao cliente.
- Aplica sanitização transparente no payload (injetando `skip_thought_signature_validator` ou limpando blocos de HMAC incompatíveis) preservando todo o histórico de conversação do usuário.
- Reenvia a requisição imediatamente em memória para o Google Cloud Code PA, garantindo que o agente continue pensando sem travar o editor ou interromper a sessão de código.

### 4. Proxy de Saída Personalizado por Conta (Suporte Webshare / Proxies Residenciais)
Para evitar limites de taxa baseados em IP entre múltiplas contas Google, cada conta no pool pode receber seu próprio proxy HTTP/HTTPS de saída dedicado (ex.: `http://usr123:pass@p.webshare.io:80`):
- **Isolamento de IP por Conta**: Ao despachar requisições para uma conta específica, o switcher roteia o tráfego do Google Cloud Code PA através do proxy configurado para aquela conta.
- **Configuração Fácil**: Edite ou remova a URL do proxy de saída diretamente no card da conta no Dashboard Web clicando em **Edit Proxy**.
- **Ofuscação Segura de Senhas**: Credenciais de proxy de saída são ofuscadas com segurança na interface web (e completamente ocultas no Modo Privacidade).

---

## Referência de Configuração

As configurações ficam salvas em formato JSON em `~/.config/antigravity-account-switcher/config.json`.

```bash
# Ver todas as configurações atuais
antigravity-account-switcher config list

# Definir caminho do executável do Antigravity 2.0
antigravity-account-switcher config set antigravity_bin ~/.local/share/antigravity/antigravity

# Definir porta padrão do dashboard web
antigravity-account-switcher config set port 1831

# Ajustar intervalo de checagem de cotas em segundo plano
antigravity-account-switcher config set quota_interval 60s

# Configurar modelos primário e secundário de contingência
antigravity-account-switcher config set model_primary gemini-2.5-pro
antigravity-account-switcher config set model_secondary claude-3-7-sonnet

# Habilitar o fallback intra-conta antes de alternar de conta
antigravity-account-switcher config set fallback_secondary_enabled true
```

### Variáveis de Ambiente

| Variável | Descrição |
| :--- | :--- |
| `ANTIGRAVITY_BIN` | Caminho explícito para o executável do Antigravity 2.0. |
| `ANTIGRAVITY_PORT` | Sobrescreve a porta do proxy e dashboard. |
| `ANTIGRAVITY_DB_PATH` | Caminho para o banco de dados SQLite (padrão: `~/.config/.../accounts.db`). |
| `ANTIGRAVITY_CLIENT_ID` | Sobrescrita opcional do Client ID do Google Cloud Console. |
| `ANTIGRAVITY_CLIENT_SECRET` | Sobrescrita opcional do Client Secret do Google Cloud Console. |
| `ANTIGRAVITY_MODEL_PRIMARY` | Sobrescreve o modelo primário configurado. |
| `ANTIGRAVITY_MODEL_SECONDARY` | Sobrescreve o modelo secundário de contingência. |
| `ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED` | Habilita/desabilita o fallback intra-conta (`true`/`false`). |

---

## Arquitetura

```text
               +-----------------------------------+
               |          Antigravity 2.0          |
               |    (Processo Filho via Supervisor)|
               +-----------------+-----------------+
                                 | HTTP_PROXY / CLOUD_CODE_URL
                                 v
+------------------------------------------------------------------+
|              ANTIGRAVITY ACCOUNT SWITCHER (Binário Único)        |
|                                                                  |
|   +--------------------+     +-------------------------------+   |
|   |   Dashboard Web    |     |     Proxy Reverso em Processo |   |
|   |  (HTML5/Tailwind)  |     | * Bearer Token Dinâmico       |   |
|   |  http://127.0.0.1  |     | * Buffer de Replay de 100MB   |   |
|   +---------+----------+     | * Túnel RFC 7231 CONNECT      |   |
|             |                +---------------+---------------+   |
|             v                                |                   |
|   +--------------------+         HTTP 429    | Tokens SSE        |
|   | Banco SQLite WAL   |<--------------------+                   |
|   | * accounts.db      |                     v                   |
|   | * métricas tokens  |     +-------------------------------+   |
|   +---------+----------+     | Daemon de Monitor de Cotas    |   |
|             ^                | * Auto-restaura após o reset  |   |
|             +----------------+ * User-Agent oficial Google PA|   |
+----------------------------------------------+-------------------+
                                               |
                                               v
                              +--------------------------------+
                              | Infraestrutura Google CloudCode|
                              +--------------------------------+
```

---

## Solução de Problemas & Perguntas Frequentes (FAQ)

#### 1. "Could not automatically locate Antigravity binary"
Se a sua instalação do Antigravity 2.0 estiver em um diretório personalizado que não foi detectado automaticamente, aponte para o executável:
```bash
antigravity-account-switcher config set antigravity_bin /caminho/para/antigravity
```

#### 2. Isso interfere na digitação por voz (Speech-to-Text) do Antigravity?
Não. O tráfego de voz (`speech.googleapis.com`) passa por um túnel TCP bruto via RFC 7231 em conexões `CONNECT`, byte a byte, então o áudio não é inspecionado nem alterado. Como todo tráfego que não é loopback, o túnel sai pelo proxy da conta ativa, e a digitação por voz nunca revela seu IP real.

#### 3. Onde ficam guardados meus tokens e credenciais?
Os tokens ficam armazenados exclusivamente no seu disco local, no banco SQLite protegido em `~/.config/antigravity-account-switcher/accounts.db`. Nenhuma informação, token ou métrica jamais sai da sua máquina.

#### 4. Como faço para desinstalar completamente e apagar todos os dados?
```bash
# 1. Remove o atalho do menu desktop
antigravity-account-switcher uninstall-desktop

# 2. Remove o binário
make uninstall

# 3. Exclui a pasta de configurações e o banco de dados
rm -rf ~/.config/antigravity-account-switcher
```

#### 5. A autoatualização nativa ("Check for Updates") funciona?
Sim! No Linux, o Antigravity 2.0 utiliza o mecanismo `AppImageUpdater` do Electron. Ao rodar o Antigravity a partir do arquivo `.tar.gz` descompactado sem um runtime de AppImage, a opção `Help -> Check for Updates` costuma falhar com `ERR_UPDATER_OLD_FILE_NOT_FOUND` pela ausência da variável de ambiente `APPIMAGE`.

**O Antigravity Account Switcher corrige isso de fábrica.** Ao iniciar via `antigravity-account-switcher launch` (ou pelo atalho desktop criado por `install-desktop`), o supervisor injeta automaticamente a variável `APPIMAGE` apontando para o binário correto no processo do Antigravity 2.0, permitindo que a verificação de atualizações e a atualização automática funcionem perfeitamente.

---

## Segurança

Consulte [SECURITY.md](SECURITY.md) para detalhes sobre políticas de reporte de vulnerabilidades e conformidade com a especificação RFC 8252 §8.5 para clientes OAuth 2.0 públicos.

---

## Como Contribuir

Contribuições são muito bem-vindas! Consulte o arquivo [CONTRIBUTING.md](CONTRIBUTING.md) para instruções de ambiente de desenvolvimento, execução dos testes com detector de race e padrões de Pull Request.

---

## Créditos & Autor Original

Este projeto foi originalmente concebido e desenvolvido por **[Muriel Gasparini](https://github.com/Muriel-Gasparini)** ([Muriel-Gasparini/antigravity-account-switcher](https://github.com/Muriel-Gasparini/antigravity-account-switcher)).  
Mantido e atualizado por **[AugusttoDaniel](https://github.com/AugusttoDaniel)**.

---

## Licença

Distribuído sob a licença MIT © 2026 Muriel Gasparini e © 2026 Daniel Augusto Silva. Veja [LICENSE](LICENSE) para mais detalhes.
