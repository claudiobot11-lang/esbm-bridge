//go:build windows

// GUI setup path — the "no cmd" experience for store operators. When
// the .exe is double-clicked from Explorer (no console attached), all
// interaction happens through native Windows dialogs (zenity): the
// operator types the pairing code in a popup and confirms install with
// a UAC prompt. They never see cmd.exe.
package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/claudiobot11-lang/esbm-bridge/internal/api"
	"github.com/claudiobot11-lang/esbm-bridge/internal/config"
	"github.com/ncruces/zenity"
)

const guiServerDefault = "https://app.estacionsanbrunomarket.com"

// cmdSetupGUI runs the double-click-friendly setup with native dialogs.
// Returns a process exit code. Flow:
//  1. Already paired → start the tray (monitoring) and return.
//  2. Not paired → popup for the 6-digit code → pair → save.
//  3. Offer "install as service" → self-elevate (UAC) → background service.
//     If declined, run in tray for this session.
func cmdSetupGUI(log *slog.Logger) int {
	if _, err := config.Load(); err == nil {
		// Paired already — operator just wants to see status.
		return cmdTray(log, nil)
	}

	code, err := zenity.Entry(
		"Cole o código de pareamento de 6 dígitos.\n\n"+
			"Pegue em app.estacionsanbrunomarket.com → /esl/stores → \"Add Store\".\n"+
			"O código expira em 10 minutos.",
		zenity.Title("ESBM Bridge — Configuração"),
	)
	if err != nil {
		// Operator cancelled the dialog — nothing to do.
		return 0
	}
	code = strings.TrimSpace(code)
	if code == "" {
		_ = zenity.Error("Nenhum código informado.", zenity.Title("ESBM Bridge"))
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pr, perr := api.New(guiServerDefault, "").Pair(ctx, code)
	cancel()
	if perr != nil {
		_ = zenity.Error("Falha no pareamento:\n\n"+perr.Error()+
			"\n\nConfira o código e a internet, e tente de novo.",
			zenity.Title("ESBM Bridge"))
		return 1
	}

	cfg := &config.Config{
		BridgeJWT:        pr.JWT,
		TailscaleAuthKey: pr.TailscaleAuthKey,
		ShopCode:         pr.ShopCode,
		ServerAddr:       pr.ServerAddr,
		EsbmAppURL:       guiServerDefault,
	}
	cfg.Defaults()
	if err := cfg.Save(); err != nil {
		_ = zenity.Error("Falha ao salvar a configuração:\n\n"+err.Error(),
			zenity.Title("ESBM Bridge"))
		return 1
	}
	log.Info("paired via GUI", "shop_code", cfg.ShopCode)

	// Offer the unattended install (recommended). zenity.Question
	// returns nil when the operator clicks the OK button.
	q := zenity.Question(
		"Pareado como \""+cfg.ShopCode+"\". \n\n"+
			"Instalar para rodar SEMPRE em segundo plano "+
			"(inicia no boot e reinicia sozinho)?\n\n"+
			"O Windows vai pedir permissão de administrador.",
		zenity.Title("ESBM Bridge"),
		zenity.OKLabel("Instalar"),
		zenity.CancelLabel("Agora não"),
	)
	if q == nil {
		if err := relaunchElevated("install"); err != nil {
			_ = zenity.Error("Não consegui iniciar a instalação:\n\n"+err.Error()+
				"\n\nO Bridge ainda vai rodar enquanto esta janela estiver aberta.",
				zenity.Title("ESBM Bridge"))
			return cmdTray(log, nil)
		}
		_ = zenity.Info(
			"Confirme o pedido de administrador do Windows.\n\n"+
				"Depois disso o Bridge roda sozinho em segundo plano — "+
				"pode fechar tudo. Status em app.estacionsanbrunomarket.com → /esl.",
			zenity.Title("ESBM Bridge — Instalado"))
		return 0
	}

	// Declined install → run in tray so it works at least this session.
	_ = zenity.Info(
		"OK. O Bridge vai rodar enquanto esta janela/ícone estiver aberto.\n\n"+
			"Pra rodar sempre, abra o programa de novo e escolha Instalar.",
		zenity.Title("ESBM Bridge"))
	return cmdTray(log, nil)
}
