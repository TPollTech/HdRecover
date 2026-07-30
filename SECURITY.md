# Política de segurança

## Versões

| Versão | Situação |
|---|---|
| `0.5.4-rc.1` | Em validação; correções de segurança aceitas |
| `0.5.3` e anteriores | Preservadas, sem aprovação para uso profissional |

## Como reportar

Use o recurso **Security Advisories** do GitHub deste repositório para enviar detalhes de forma privada. Se ele não estiver habilitado, abra uma issue sem anexar dados de clientes, imagens de disco, chaves, tokens, seriais completos ou prova de conceito explorável; peça um canal privado ao mantenedor.

Inclua:

- versão ou commit;
- versão do Windows e arquitetura;
- modo utilizado;
- resultado esperado e observado;
- passos mínimos usando dados sintéticos;
- impacto possível.

## Operações destrutivas

Falhas de seleção de disco, validação de identidade, bloqueio de volume, BitLocker, VSS, GPT/MBR ou retomada são tratadas como vulnerabilidades de alta prioridade. Não teste relatos em discos com dados únicos.

## Segredos

Não adicione tokens, chaves, imagens de clientes, relatórios com dados pessoais nem dumps de disco ao repositório. Revogue imediatamente qualquer segredo publicado por engano.
