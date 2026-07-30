# Changelog

Todas as mudanças relevantes deste projeto serão registradas neste arquivo.

## [0.5.4-rc.1] — 2026-07-30

### Restaurado

- Código-fonte completo publicado como árvore normal do Git.
- Motor de recuperação, testes, clonagem, VSS e migração inteligente recuperados da última fonte íntegra preservada.

### Segurança

- Identificação de discos enriquecida com serial, `UniqueId`, tamanho, setor lógico e funções de boot/sistema.
- Origem e destino revalidados imediatamente antes da operação e novamente antes de abrir o destino.
- Destinos com função de boot/sistema ou somente leitura são bloqueados.
- Clonagem offline falha de modo seguro se qualquer volume da origem não puder ser bloqueado e desmontado.
- Migração VSS exige cobertura de todos os volumes e BitLocker totalmente descriptografado.
- Plano de redução é reanalisado antes de alterar a origem e possui script de restauração com verificação de identidade.
- Retomada usa identidade de hardware, checkpoints duráveis e substituição atômica do estado no Windows.
- GPT primária passa por validação de CRC do cabeçalho e da tabela de entradas antes de qualquer ajuste.
- Partições ocultas genéricas não são mais classificadas automaticamente como recuperação descartável.

### Infraestrutura

- Removidos o pacote `.tar.xz` corrompido e os workflows de bootstrap com escrita automática.
- CI somente leitura com formatação, testes, race detector, `go vet`, `govulncheck` e builds Windows 32/64 bits.
- Artefatos de CI recebem hashes SHA-256 e não são adicionados ao repositório.

### Documentação

- Adicionados README, política de segurança, guia de clonagem e relatório de remediação.

### Pendente para versão estável

- Testes físicos controlados em hardware descartável.
- Matriz real de boot GPT/UEFI, MBR/BIOS, SSD menor, setores 512/4K, falhas de leitura e interrupção de energia.
