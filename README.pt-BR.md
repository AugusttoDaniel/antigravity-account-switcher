# Antigravity Account Switcher

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
