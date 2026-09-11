package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kuri4/dockerherocker/docker"
	"github.com/mattn/go-runewidth"
)

// menuItem is a single action in the container context menu. key is the
// single-letter hotkey shown highlighted right before the label. activate
// returns the tea.Cmd to dispatch (nil keeps the popup open). confirm marks
// a staging item (Remove variants) whose activation swaps the popup into the
// destructive-action confirm stage instead of dispatching anything;
// removeVolumes selects the variant that removes the container with its data.
// children, when non-empty, turn the item into a submenu parent: activating
// it pushes the current level onto the popup's stack and shows its children.
type menuItem struct {
	label         string
	key           string
	cli           string // equivalent docker CLI command (shown right-aligned, no target for bulk actions)
	children      []menuItem
	confirm       bool
	removeVolumes bool   // pairs with confirm: "Remove with data" variant
	confirmHeader string // confirm-stage header (used outside the Containers tab)
	activate      func() tea.Cmd
}

// popupMenu is the container context menu, centered on the screen. x,y holds
// the 0-based cell of its top-left corner; w,h its box size in cells (already
// clamped to the terminal). confirm marks the second, destructive stage.
// stack holds the parent item lists of any open submenu (deepest last) so Esc
// can walk back up to the root instead of instantly closing.
type popupMenu struct {
	items         []menuItem
	stack         [][]menuItem
	stackDividers [][]int // divider layout per open submenu level (deepest last)
	dividers      []int   // item indices after which a horizontal separator is drawn
	x, y          int
	w, h          int
	sel           int
	confirm       bool
	header        string
}

// menuMinWidth is the minimum content width (cells inside the borders) of the
// popup, so a short two-item menu does not hug its labels; the confirm-stage
// header widens the box further whenever it is longer.
const menuMinWidth = 16

// menuCliGap is the minimum number of cells left between the action label and
// the right-aligned docker CLI hint.
const menuCliGap = 2

// menuCliWidth returns the width of the widest docker CLI hint among the
// items, or 0 when no item carries one (so popups without hints keep their
// compact sizing).
func menuCliWidth(items []menuItem) int {
	n := 0
	for _, it := range items {
		if w := runewidth.StringWidth(it.cli); w > n {
			n = w
		}
	}
	return n
}

// containerMenuItems builds the first-stage actions for a container: a
// state-dependent Stop/Pause or Start, a restart in all states, plus a Remove
// item that expands into a submenu (Remove and Remove-with-data variants).
// Hotkeys are only meaningful at this top level; once the user drills into the
// submenu or the confirm stage they disappear.
func (m Model) containerMenuItems(c docker.Container) ([]menuItem, []int) {
	name := containerDisplayName(c)
	var items []menuItem
	switch c.State {
	case "running":
		items = append(items,
			menuItem{label: "Stop", key: "s", cli: "docker stop " + name, activate: m.toggleContainer},
			menuItem{label: "Pause", key: "p", cli: "docker pause " + name, activate: m.pauseContainer},
			menuItem{label: "Exec shell", key: "e", cli: "docker exec -it " + name + " sh", activate: m.execShell},
			menuItem{label: "Attach", key: "t", cli: "docker attach --sig-proxy=false " + name, activate: m.attachContainer},
		)
	case "paused":
		items = append(items,
			menuItem{label: "Resume", key: "r", cli: "docker unpause " + name, activate: m.resumeContainer},
		)
	default:
		items = append(items, menuItem{label: "Start", key: "s", cli: "docker start " + name, activate: m.toggleContainer})
	}
	items = append(items,
		menuItem{label: "Restart", key: "r", cli: "docker restart " + name, activate: m.restartContainer},
	)
	remove := menuItem{
		label: "Remove",
		key:   "d",
		children: []menuItem{
			{label: "Remove", confirm: true, cli: "docker rm " + name},
			{label: "Remove with data", confirm: true, removeVolumes: true, cli: "docker rm -v " + name},
		},
	}
	items = append(items, remove)
	var dividers []int
	if m.hasStoppedContainers() {
		// Bulk action: acts on every stopped container, visually separated
		// from the single-container operations above.
		dividers = append(dividers, len(items))
		items = append(items, menuItem{
			label:         "Prune stopped",
			key:           "g",
			cli:           "docker container prune", // daemon-wide, no target
			confirm:       true,
			confirmHeader: "Prune all stopped containers?",
			activate:      m.pruneContainers,
		})
	}
	return items, dividers
}

// hasStoppedContainers reports whether the current list contains any container
// the daemon would prune: exited, created, or dead (everything not running,
// paused or restarting). Gate the "Prune stopped" action on this so it never
// appears for a list that only holds active containers.
func (m Model) hasStoppedContainers() bool {
	for _, c := range m.containers {
		switch c.State {
		case "running", "paused", "restarting":
			continue
		default:
			return true
		}
	}
	return false
}

// enterConfirmStage swaps the popup into the destructive-action stage: a
// header naming the target plus Yes/No items. yes is the tea.Cmd to dispatch
// when the user confirms; nil-returning no-op keeps the popup open and is
// closed by menuActivate. The selection resets to Yes so a deliberate second
// Enter executes; Esc still cancels.
func (m *Model) enterConfirmStage(header string, yes func() tea.Cmd) {
	m.menu.confirm = true
	m.menu.sel = 0
	m.menu.stack = nil
	m.menu.stackDividers = nil
	m.menu.dividers = nil
	m.menu.header = header
	m.menu.items = []menuItem{
		{label: "Yes, remove", activate: yes},
		{label: "No, cancel", activate: func() tea.Cmd { return nil }},
	}
	m.menu.w, m.menu.h = menuMeasure(m.menu.items, dividerCount(m.menu.dividers), m.menu.header)
	// keep the enlarged confirm box centered like the first stage
	m.menu.x = max((m.width-m.menu.w)/2, 0)
	m.menu.y = max((m.height-m.menu.h)/2, tabBarHeight+1)
}

// runDockerOp wraps a docker operation into the standard settle-then-refresh
// cycle every menu action uses: failures surface through the toast, a short
// settle delay lets the daemon propagate the change, then the lists refresh.
// guard, when non-nil, is evaluated up front — a false result means the
// command is stale (e.g. the tab switched away) and the whole action is
// dropped, mirroring the pre-checks the callbacks used to inline.
func (m Model) runDockerOp(guard func() bool, op func() error) tea.Cmd {
	if guard != nil && !guard() {
		return nil
	}
	return func() tea.Msg {
		if err := op(); err != nil {
			return errMsg{err}
		}
		time.Sleep(500 * time.Millisecond)
		return m.refreshNow()()
	}
}

// removeContainer / removeContainerVolumes remove the selected container
// (with its volumes) and refresh the list, mirroring toggleContainer and
// restartContainer.
func (m Model) removeContainer() tea.Cmd        { return m.removeContainerCmd(false) }
func (m Model) removeContainerVolumes() tea.Cmd { return m.removeContainerCmd(true) }

func (m Model) removeContainerCmd(volumes bool) tea.Cmd {
	c, ok := m.selectedContainer()
	if !ok {
		return nil
	}
	return m.runDockerOp(nil, func() error {
		if volumes {
			return m.docker.RemoveContainerVolumes(c.ID)
		}
		return m.docker.RemoveContainer(c.ID)
	})
}

// containerDisplayName returns the leading (dash-stripped) container name,
// falling back to the raw ID when the container has none.
func containerDisplayName(c docker.Container) string {
	for _, n := range c.Names {
		if n != "" {
			return strings.TrimPrefix(n, "/")
		}
	}
	return c.ID
}

// buildContainerMenu builds the popup for the selected container, centered on
// the screen and clamped into the terminal, keeping it clear of the tab bar.
// The keyboard's x opens it; there is no mouse anchor anymore.
func (m Model) buildContainerMenu() popupMenu {
	c := m.containers[m.selectedIdx]
	items, dividers := m.containerMenuItems(c)
	return m.buildPopupMenu("Actions for container "+containerDisplayName(c), items, dividers)
}

// buildImageMenu builds the popup for the selected image, centered on the
// screen like the container menu. The keyboard x opens it on the Images tab.
func (m Model) buildImageMenu() popupMenu {
	img := m.images[m.selectedIdx]
	items, dividers := m.imageMenuItems(img)
	return m.buildPopupMenu("Actions for image "+imageDisplayName(img), items, dividers)
}

// buildVolumeMenu builds the popup for the selected volume, centered on the
// screen like the container and image menus. The keyboard x opens it on the
// Volumes tab.
func (m Model) buildVolumeMenu() popupMenu {
	v := m.volumes[m.selectedIdx]
	items, dividers := m.volumeMenuItems(v)
	return m.buildPopupMenu("Actions for volume "+v.Name, items, dividers)
}

// buildNetworkMenu builds the popup for the selected network, centered on the
// screen like the other tab menus. The keyboard x opens it on the Networks
// tab.
func (m Model) buildNetworkMenu() popupMenu {
	n := m.networks[m.selectedIdx]
	items, dividers := m.networkMenuItems(n)
	return m.buildPopupMenu("Actions for network "+n.Name, items, dividers)
}

// buildComposeMenu builds the popup for the selected compose project on the
// Projects tab. The keyboard x opens it; from here the project can be brought
// up, restarted, brought down (with or without volume removal) or have its
// logs streamed in the floating terminal. proj is the project's index in the
// (flat-selection) compose list.
func (m Model) buildComposeMenu(proj int) popupMenu {
	p := m.compose[proj]
	items, dividers := m.composeMenuItems(p)
	return m.buildPopupMenu("Actions for project "+p.Name, items, dividers)
}

// buildComposeServiceMenu builds the popup for a service row of an expanded
// project. proj/svc index into m.compose; the menu mirrors the container menu
// (Stop/Start, Restart, Exec shell) plus a streaming Logs action, all scoped
// to the single service.
func (m Model) buildComposeServiceMenu(proj, svc int) popupMenu {
	p := m.compose[proj]
	s := p.Services[svc]
	items, dividers := m.composeServiceMenuItems(p, s)
	return m.buildPopupMenu("Actions for service "+s.Name, items, dividers)
}

// composeMenuItems lays out the project-level actions: up/restart/down are
// non-destructive lifecycle ops; "down -v" is marked destructive so it stages
// a confirm, and logs opens the streaming terminal. A divider separates the
// safe lifecycle group from the logs action.
func (m Model) composeMenuItems(p docker.ComposeProject) ([]menuItem, []int) {
	items := []menuItem{
		{
			label:    "Up -d",
			key:      "u",
			cli:      "docker compose up -d",
			activate: func() tea.Cmd { return m.composeUp(p) },
		},
		{
			label:    "Restart",
			key:      "r",
			cli:      "docker compose restart",
			activate: func() tea.Cmd { return m.composeRestart(p) },
		},
		{
			label:    "Down",
			key:      "d",
			cli:      "docker compose down",
			activate: func() tea.Cmd { return m.composeDown(p, false) },
		},
		{
			label:         "Down -v",
			key:           "v",
			cli:           "docker compose down --volumes",
			confirm:       true,
			confirmHeader: "Down " + p.Name + " and remove its volumes?",
			activate:      func() tea.Cmd { return m.composeDown(p, true) },
		},
	}
	dividers := []int{len(items)}
	items = append(items, menuItem{
		label:    "Logs",
		key:      "l",
		cli:      "docker compose logs -f",
		activate: func() tea.Cmd { return m.composeLogs(p) },
	})
	return items, dividers
}

// composeUp / composeRestart / composeDown run the CLI file operations for the
// selected project and refresh the list, mirroring the other tab actions.
func (m Model) composeUp(p docker.ComposeProject) tea.Cmd {
	return m.runDockerOp(func() bool { return m.activeTab == tabCompose }, func() error { return m.docker.ComposeUp(p) })
}

func (m Model) composeRestart(p docker.ComposeProject) tea.Cmd {
	return m.runDockerOp(func() bool { return m.activeTab == tabCompose }, func() error { return m.docker.ComposeRestart(p) })
}

func (m Model) composeDown(p docker.ComposeProject, volumes bool) tea.Cmd {
	return m.runDockerOp(func() bool { return m.activeTab == tabCompose }, func() error { return m.docker.ComposeDown(p, volumes) })
}

// composeLogs opens the streaming `docker compose logs -f` session for the
// project in the floating terminal.
func (m Model) composeLogs(p docker.ComposeProject) tea.Cmd {
	return m.launchTerminal(m.docker.ComposeLogsArgs(p)...)
}

// composeServiceMenuItems lays out the per-service actions, mirroring the
// container menu: a state-aware Stop/Start, Restart in every state, and Exec
// shell only while running (docker compose exec needs a live container). The
// streaming Logs action is separated behind a divider like in the project menu.
func (m Model) composeServiceMenuItems(p docker.ComposeProject, s docker.ComposeService) ([]menuItem, []int) {
	var items []menuItem
	if s.Running {
		items = append(items,
			menuItem{label: "Stop", key: "s", cli: "docker compose stop " + s.Name, activate: func() tea.Cmd { return m.composeServiceStop(p, s) }},
			menuItem{label: "Restart", key: "r", cli: "docker compose restart " + s.Name, activate: func() tea.Cmd { return m.composeServiceRestart(p, s) }},
			menuItem{label: "Exec shell", key: "e", cli: "docker compose exec " + s.Name + " sh", activate: func() tea.Cmd { return m.composeServiceExec(p, s) }},
		)
	} else {
		items = append(items,
			menuItem{label: "Start", key: "s", cli: "docker compose up -d " + s.Name, activate: func() tea.Cmd { return m.composeServiceUp(p, s) }},
			menuItem{label: "Restart", key: "r", cli: "docker compose restart " + s.Name, activate: func() tea.Cmd { return m.composeServiceRestart(p, s) }},
		)
	}
	dividers := []int{len(items)}
	items = append(items, menuItem{
		label:    "Logs",
		key:      "l",
		cli:      "docker compose logs -f " + s.Name,
		activate: func() tea.Cmd { return m.composeServiceLogs(p, s) },
	})
	return items, dividers
}

// composeServiceUp / composeServiceRestart / composeServiceStop run the CLI
// service-scoped operations and refresh the list, mirroring the project ops.
func (m Model) composeServiceUp(p docker.ComposeProject, s docker.ComposeService) tea.Cmd {
	return m.runDockerOp(func() bool { return m.activeTab == tabCompose }, func() error { return m.docker.ComposeServiceUp(p, s.Name) })
}

func (m Model) composeServiceRestart(p docker.ComposeProject, s docker.ComposeService) tea.Cmd {
	return m.runDockerOp(func() bool { return m.activeTab == tabCompose }, func() error { return m.docker.ComposeServiceRestart(p, s.Name) })
}

func (m Model) composeServiceStop(p docker.ComposeProject, s docker.ComposeService) tea.Cmd {
	return m.runDockerOp(func() bool { return m.activeTab == tabCompose }, func() error { return m.docker.ComposeServiceStop(p, s.Name) })
}

// composeServiceLogs opens `docker compose logs -f <svc>` for the single
// service in the floating terminal.
func (m Model) composeServiceLogs(p docker.ComposeProject, s docker.ComposeService) tea.Cmd {
	return m.launchTerminal(m.docker.ComposeServiceLogsArgs(p, s.Name)...)
}

// composeServiceExec opens `docker compose exec <svc> sh` in the floating
// terminal, mirroring the container's exec shell action.
func (m Model) composeServiceExec(p docker.ComposeProject, s docker.ComposeService) tea.Cmd {
	return m.launchTerminal(m.docker.ComposeServiceExecArgs(p, s.Name)...)
}

// buildPopupMenu packages a header, the item list and their divider layout
// into a centered popup box, clamped into the terminal and kept clear of the
// tab bar. Every tab menu goes through here so sizing and placement stay the
// same across containers, images, volumes and networks.
func (m Model) buildPopupMenu(header string, items []menuItem, dividers []int) popupMenu {
	w, h := menuMeasure(items, dividerCount(dividers), header)
	return popupMenu{
		items:    items,
		dividers: dividers,
		header:   header,
		x:        max((m.width-w)/2, 0),
		y:        max((m.height-h)/2, tabBarHeight+1),
		w:        w,
		h:        h,
	}
}

// removeItem is the single-resource destructive action every status-aware tab
// menu shares: "Remove" for unused resources, "Force remove" (the docker CLI
// gains a -f flag) when the daemon would reject the plain removal — images and
// volumes. Networks are never forceable, so they call this with force=false.
// The item stages a destructive confirm whose header names the target, and
// activate carries the force decision into the removal command.
func (m Model) removeItem(cmd, name string, force bool, activate func(bool) tea.Cmd) menuItem {
	label := "Remove"
	cli := cmd + " " + name
	if force {
		label = "Force remove"
		cli = cmd + " -f " + name
	}
	return menuItem{
		label:         label,
		key:           "d",
		cli:           cli,
		confirm:       true,
		confirmHeader: label + " " + name + "?",
		activate:      func() tea.Cmd { return activate(force) },
	}
}

// appendBulkAction appends the gated bulk action (prune) to a menu behind a
// horizontal divider, or returns the items untouched when the list holds no
// prune candidate. Image, volume and network menus all lay out this way.
func appendBulkAction(items []menuItem, gate bool, bulk menuItem) ([]menuItem, []int) {
	var dividers []int
	if gate {
		dividers = append(dividers, len(items))
		items = append(items, bulk)
	}
	return items, dividers
}

// imageMenuItems builds the first-stage actions for an image, adapting to its
// status: an image in use does not allow a plain removal (the daemon rejects
// it), so it only offers "Force remove" (untag); unused and dangling images
// offer "Remove". "Prune dangling" cleans every dangling image and is shown
// only when at least one exists in the current list, separated by a divider
// because it is a bulk action.
func (m Model) imageMenuItems(img docker.Image) ([]menuItem, []int) {
	inUse := classifyImage(img.Containers, len(img.RepoTags)).label == "IN-USE"
	name := imageDisplayName(img)

	hasDangling := false
	for _, im := range m.images {
		if len(im.RepoTags) == 0 {
			hasDangling = true
			break
		}
	}

	items := []menuItem{
		m.removeItem("docker rmi", name, inUse, func(force bool) tea.Cmd { return m.imageRemoveCmd(img.ID, force) }),
	}
	bulk := menuItem{
		label:         "Prune dangling",
		key:           "p",
		cli:           "docker image prune", // daemon-wide, no target
		confirm:       true,
		confirmHeader: "Prune all dangling images?",
		activate:      m.pruneImagesCmd,
	}
	return appendBulkAction(items, hasDangling, bulk)
}

// imageDisplayName returns the first tag of an image, falling back to the
// short sha256 digest when the image is dangling (no tags).
func imageDisplayName(img docker.Image) string {
	if len(img.RepoTags) > 0 {
		return img.RepoTags[0]
	}
	if len(img.ID) > 7 && img.ID[:7] == "sha256:" {
		return "sha256:" + img.ID[7:19]
	}
	if n := len(img.ID); n > 12 {
		return img.ID[:12]
	}
	return img.ID
}

// volumeMenuItems builds the first-stage actions for a volume, adapting to its
// status like the image menu does: an in-use volume cannot be removed plainly
// (the daemon rejects it), so it only offers "Force remove"; unused volumes
// offer "Remove". "Prune unused" cleans every UNUSED volume and is shown only
// when at least one exists in the current list, separated by a divider because
// it is a bulk action.
func (m Model) volumeMenuItems(v docker.Volume) ([]menuItem, []int) {
	inUse := classifyUsage(v.RefCount).label == "IN-USE"

	items := []menuItem{
		m.removeItem("docker volume rm", v.Name, inUse, func(force bool) tea.Cmd { return m.volumeRemoveCmd(v.Name, force) }),
	}
	bulk := menuItem{
		label:         "Prune unused",
		key:           "p",
		cli:           "docker volume prune -a", // daemon-wide, no target
		confirm:       true,
		confirmHeader: "Prune all unused volumes?",
		activate:      m.pruneVolumesCmd,
	}
	return appendBulkAction(items, m.hasUnusedVolumes(), bulk)
}

// hasUnusedVolumes reports whether the current list contains any volume with
// no container reference (what `docker volume prune -a` would remove). Gate
// the "Prune unused" action on this so it never appears for a list that only
// holds in-use volumes.
func (m Model) hasUnusedVolumes() bool {
	for _, v := range m.volumes {
		if v.RefCount <= 0 {
			return true
		}
	}
	return false
}

// imageRemoveCmd / pruneImagesCmd act on the selected image (or all dangling
// images) and refresh the list, mirroring removeContainerCmd.
func (m Model) imageRemoveCmd(id string, force bool) tea.Cmd {
	return m.runDockerOp(nil, func() error { return m.docker.RemoveImage(id, force) })
}

func (m Model) pruneImagesCmd() tea.Cmd {
	return m.runDockerOp(nil, func() error { return m.docker.PruneImages() })
}

// pruneContainers prunes every stopped container the daemon still holds and
// refreshes the list, mirroring pruneImagesCmd. The popup is already in the
// confirm stage when this is bound, so it runs only after an explicit Yes.
func (m Model) pruneContainers() tea.Cmd {
	return m.runDockerOp(func() bool { return m.docker != nil }, func() error { return m.docker.PruneContainers() })
}

// volumeRemoveCmd removes the selected volume (with force for an in-use one)
// and refreshes the list, mirroring imageRemoveCmd.
func (m Model) volumeRemoveCmd(name string, force bool) tea.Cmd {
	return m.runDockerOp(func() bool { return m.activeTab == tabVolumes }, func() error { return m.docker.RemoveVolume(name, force) })
}

// pruneVolumesCmd prunes every unused volume the daemon still holds and
// refreshes the list, mirroring pruneImagesCmd.
func (m Model) pruneVolumesCmd() tea.Cmd {
	return m.runDockerOp(func() bool { return m.docker != nil }, func() error { return m.docker.PruneVolumes() })
}

// networkMenuItems builds the first-stage actions for a network. Remove is
// always offered regardless of status — there is no force variant for
// networks, so the daemon refuses an in-use network and that error surfaces in
// the toast. "Prune unused" is shown only when the list holds at least one
// network no container is attached to.
func (m Model) networkMenuItems(n docker.Network) ([]menuItem, []int) {
	items := []menuItem{
		m.removeItem("docker network rm", n.Name, false, func(force bool) tea.Cmd { return m.networkRemoveCmd(n.Name) }),
	}
	bulk := menuItem{
		label:         "Prune unused",
		key:           "p",
		cli:           "docker network prune", // daemon-wide, no target
		confirm:       true,
		confirmHeader: "Prune all unused networks?",
		activate:      m.pruneNetworksCmd,
	}
	return appendBulkAction(items, m.hasUnusedNetworks(), bulk)
}

// hasUnusedNetworks reports whether the current list contains any network no
// container is attached to (what `docker network prune` would remove). Gate
// the "Prune unused" action on this so it never appears for a list that only
// holds in-use networks.
func (m Model) hasUnusedNetworks() bool {
	for _, n := range m.networks {
		if n.Containers <= 0 {
			return true
		}
	}
	return false
}

// networkRemoveCmd removes the selected network and refreshes the list,
// mirroring volumeRemoveCmd.
func (m Model) networkRemoveCmd(name string) tea.Cmd {
	return m.runDockerOp(func() bool { return m.activeTab == tabNetworks }, func() error { return m.docker.RemoveNetwork(name) })
}

// pruneNetworksCmd prunes every unused network the daemon still holds and
// refreshes the list, mirroring pruneVolumesCmd.
func (m Model) pruneNetworksCmd() tea.Cmd {
	return m.runDockerOp(func() bool { return m.docker != nil }, func() error { return m.docker.PruneNetworks() })
}

func menuMeasure(items []menuItem, dividers int, header string) (w, h int) {
	w = 2               // left + right border cells
	const hotkeyPad = 2 // "<letter> " prefix before each hotkeyed label
	cliW := menuCliWidth(items)
	for _, it := range items {
		l := runewidth.StringWidth(it.label)
		if it.key != "" {
			l += hotkeyPad
		}
		if it.children != nil {
			l += 2 // trailing submenu arrow " ›"
		}
		if cliW > 0 {
			l += menuCliGap + cliW + 1 // right-aligned docker CLI hint column + trailing gap
		}
		if l > w {
			w = l
		}
	}
	if header != "" {
		// the title row reserves a leading and a trailing cell of breathing
		// room, so it is one wider than the header text itself.
		if l := runewidth.StringWidth(header) + 2; l > w {
			w = l
		}
	}
	w = max(w, menuMinWidth)
	w += 3 // inside padding: one leading indent cell per text row + filler
	h = len(items) + 2 + dividers
	if header != "" {
		h += 2 // title row + horizontal separator below it
	}
	return w, h
}

// dividerCount reports how many horizontal separator rows the popup needs to
// draw for the given divider positions.
func dividerCount(dividers []int) int {
	return len(dividers)
}

// menuHasDivider reports whether a horizontal separator row precedes the item
// at index i.
func menuHasDivider(dividers []int, i int) bool {
	for _, d := range dividers {
		if d == i {
			return true
		}
	}
	return false
}

// renderContainerMenu draws the popup box as one string per screen row, each
// exactly w cells wide and carrying its own surface/selection background, so
// the overlay can block what the app drew underneath. Text rows are indented
// by one leading space so labels do not touch the border.
func (m Model) renderContainerMenu() []string {
	iw := max(m.menu.w-2, 0)
	rows := []string{MenuBoxStyle.Render("┌" + strings.Repeat("─", iw) + "┐")}
	if m.menu.header != "" {
		// Reserve a leading and a trailing space in the title row so the text
		// always sits clear of both borders (right gap of at least 1 cell).
		// The header text itself is drawn orange (MenuTitleStyle) while the
		// surrounding filler keeps the box's surface background.
		rows = append(rows,
			MenuBoxStyle.Render("│ ")+
				MenuTitleStyle.Render(centerMenuRunes(m.menu.header, max(iw-2, 0)))+
				MenuBoxStyle.Render(" │"))
		rows = append(rows, MenuBoxStyle.Render("│"+strings.Repeat("─", iw)+"│"))
	}
	for i, it := range m.menu.items {
		style := MenuItemStyle
		keyStyle := MenuKeyStyle
		if i == m.menu.sel {
			style = MenuActiveItemStyle
			keyStyle = MenuActiveKeyStyle
		}
		// A horizontal rule separates the single-item actions from the bulk
		// ones; it is drawn as its own non-interactive row before the item
		// whose index is listed in dividers.
		if menuHasDivider(m.menu.dividers, i) {
			rows = append(rows, MenuBoxStyle.Render("│"+strings.Repeat("─", iw)+"│"))
		}
		label := it.label
		if it.children != nil {
			label += " ›"
		}
		// Item rows read "s Stop": a highlighted single hotkey letter (when
		// any) then the right-padded label, and when the item maps to a docker
		// CLI command a muted hint right-aligned beyond the label. Every
		// segment — the surrounding spaces, the label and the hint — is its own
		// Render on the row background, so no gap falls through to the default
		// terminal background: only the letter itself carries the
		// accent/hotkey foreground. Submenu and confirm levels carry no hotkey
		// and render as plain rows.
		cliW := menuCliWidth(m.menu.items)
		cliStyle := MenuCliStyle
		if i == m.menu.sel {
			cliStyle = MenuActiveCliStyle
		}
		var row string
		switch {
		case it.key != "":
			// labelAvail leaves room for " key " plus the reserved CLI column
			// and a trailing cell between the hint and the right border.
			labelAvail := max(iw-3-menuCliGap-cliW-1, 0)
			row = style.Render(" ") + keyStyle.Render(it.key) + style.Render(" ") +
				style.Render(padMenuRunes(label, labelAvail)) +
				style.Render(strings.Repeat(" ", menuCliGap)) +
				cliStyle.Render(padMenuRunes(it.cli, cliW)) +
				cliStyle.Render(" ")
		case cliW > 0:
			labelAvail := max(iw-1-menuCliGap-cliW-1, 0)
			row = style.Render(" "+padMenuRunes(label, labelAvail)) +
				style.Render(strings.Repeat(" ", menuCliGap)) +
				cliStyle.Render(padMenuRunes(it.cli, cliW)) +
				cliStyle.Render(" ")
		default:
			row = style.Render(" " + padMenuRunes(label, max(iw-1, 0)))
		}
		rows = append(rows, MenuBoxStyle.Render("│")+row+MenuBoxStyle.Render("│"))
	}
	rows = append(rows, MenuBoxStyle.Render("└"+strings.Repeat("─", iw)+"┘"))
	return rows
}

// padMenuRunes right-pads visible text to exactly n display cells, trimming
// with an ellipsis (fitRunes) when it is too wide.
func padMenuRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if w := runewidth.StringWidth(s); w > n {
		return fitRunes(s, n)
	}
	return s + strings.Repeat(" ", n-runewidth.StringWidth(s))
}

// centerMenuRunes pads visible text on both sides so it sits centered within
// exactly n display cells (extra cell goes to the right), trimming with an
// ellipsis (fitRunes) when it is too wide.
func centerMenuRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if w := runewidth.StringWidth(s); w > n {
		return fitRunes(s, n)
	}
	pad := n - runewidth.StringWidth(s)
	return strings.Repeat(" ", pad/2) + s + strings.Repeat(" ", pad-pad/2)
}

// menuActivate invokes the item at index i. A command means the popup closes
// and the command is dispatched (Start/Stop, "Yes, remove"); "No, cancel"
// returns nil with the popup still open and is closed here. A staging item
// (Remove variants) swaps the popup into its confirm stage and stays open.
func (m *Model) menuActivate(i int) tea.Cmd {
	if i < 0 || i >= len(m.menu.items) {
		m.menuOpen = false
		return nil
	}
	it := m.menu.items[i]
	// A parent with children is a submenu: push the current level onto the
	// stack and show its children as the new level, reselecting the first.
	// Divider layout is pushed alongside so backing out restores it.
	if len(it.children) > 0 {
		m.menu.stack = append(m.menu.stack, m.menu.items)
		m.menu.stackDividers = append(m.menu.stackDividers, m.menu.dividers)
		m.menu.items = it.children
		m.menu.dividers = nil
		m.menu.sel = 0
		m.menu.w, m.menu.h = menuMeasure(m.menu.items, dividerCount(m.menu.dividers), m.menu.header)
		m.menu.x = max((m.width-m.menu.w)/2, 0)
		m.menu.y = max((m.height-m.menu.h)/2, tabBarHeight+1)
		return nil
	}
	if it.confirm {
		switch {
		case it.confirmHeader != "":
			// Items carrying their own header act on something other than the
			// selected container (image remove, prune): just use them as-is.
			m.enterConfirmStage(it.confirmHeader, it.activate)
		case it.removeVolumes:
			c := m.containers[m.selectedIdx]
			m.enterConfirmStage("Remove "+containerDisplayName(c)+" and its volumes?", m.removeContainerVolumes)
		case m.activeTab == tabContainers:
			c := m.containers[m.selectedIdx]
			m.enterConfirmStage("Remove "+containerDisplayName(c)+"?", m.removeContainer)
		default:
			m.enterConfirmStage(it.confirmHeader, it.activate)
		}
		return nil
	}
	wasConfirm := m.menu.confirm
	cmd := it.activate()
	switch {
	case cmd != nil:
		m.menuOpen = false
		return cmd
	case wasConfirm && m.menu.confirm:
		// "No, cancel": nothing to dispatch, drop the popup.
		m.menuOpen = false
		return nil
	default:
		// No-op action; leave the popup as it is.
		return nil
	}
}

// menuSubmenuBack walks one level up the submenu stack, or closes the popup
// entirely when we are already at the root.
func (m *Model) menuSubmenuBack() {
	if n := len(m.menu.stack); n > 0 {
		m.menu.items = m.menu.stack[n-1]
		m.menu.stack = m.menu.stack[:n-1]
		m.menu.dividers = m.menu.stackDividers[n-1]
		m.menu.stackDividers = m.menu.stackDividers[:n-1]
		m.menu.sel = 0
		m.menu.w, m.menu.h = menuMeasure(m.menu.items, dividerCount(m.menu.dividers), m.menu.header)
		m.menu.x = max((m.width-m.menu.w)/2, 0)
		m.menu.y = max((m.height-m.menu.h)/2, tabBarHeight+1)
		return
	}
	m.menuOpen = false
}

// handleMenuKey processes a keystroke while the popup is open. handled=false
// (Quit only) lets the caller's normal key handling proceed so ctrl+c/q still
// quits the app; every other key closes the menu and is swallowed. A typed
// single letter that matches an item's hotkey activates that item right away;
// Esc closes an open submenu (back to its parent) before closing the popup.
func (m Model) handleMenuKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keys.Up):
		if m.menu.sel > 0 {
			m.menu.sel--
		}
		return m, nil, true
	case key.Matches(msg, keys.Down):
		if m.menu.sel < len(m.menu.items)-1 {
			m.menu.sel++
		}
		return m, nil, true
	case msg.String() == "enter":
		return m, m.menuActivate(m.menu.sel), true
	case key.Matches(msg, keys.Back):
		m.menuSubmenuBack()
		return m, nil, true
	case key.Matches(msg, keys.Quit):
		return m, nil, false
	case len(msg.Runes) == 1:
		k := string(msg.Runes[0])
		for i, it := range m.menu.items {
			if strings.EqualFold(it.key, k) {
				m.menu.sel = i
				return m, m.menuActivate(i), true
			}
		}
		m.menuOpen = false
		return m, nil, true
	default:
		m.menuOpen = false
		return m, nil, true
	}
}

// handleMenuMouse processes a mouse event while the popup is open. A left
// press inside the box acts on that item right away; releases and motions
// never close it (the press that opened it delivers a release right after);
// any other press or the wheel drops the popup.
func (m Model) handleMenuMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if msg.Type == tea.MouseLeft && msg.Action != tea.MouseActionMotion {
		sx, sy := msg.X-1, msg.Y-1 // mouse coords are 1-based screen cells
		if sx >= m.menu.x && sx < m.menu.x+m.menu.w &&
			sy >= m.menu.y && sy < m.menu.y+m.menu.h {
			row := sy - m.menu.y - 1 // below the top border
			if m.menu.header != "" {
				row -= 2 // title row + the horizontal separator below it
			}
			if idx, ok := menuRowItem(len(m.menu.items), m.menu.dividers, row); ok {
				m.menu.sel = idx
				return m, m.menuActivate(idx)
			}
			return m, nil
		}
	}
	if msg.Action == tea.MouseActionRelease || msg.Action == tea.MouseActionMotion {
		return m, nil
	}
	m.menuOpen = false
	return m, nil
}

// menuRowItem maps a 0-based row offset inside the item block (below the
// title and its separator) back to the item index, skipping the horizontal
// divider rows that separate bulk actions. ok=false for divider rows and for
// rows past the last item.
func menuRowItem(items int, dividers []int, row int) (int, bool) {
	r := 0
	for i := 0; i < items; i++ {
		if menuHasDivider(dividers, i) {
			if r == row {
				return 0, false
			}
			r++
		}
		if r == row {
			return i, true
		}
		r++
	}
	return 0, false
}

// splicePopup overlays the popup box onto the fully rendered frame, one menu
// row per target screen row, in place. It never adds rows, so the View()
// height and mouse→buffer coordinate invariants stay untouched.
func (m Model) splicePopup(content string) string {
	lines := strings.Split(content, "\n")
	for r, pr := range m.renderContainerMenu() {
		sy := m.menu.y + r
		if sy < 0 || sy >= len(lines) {
			continue
		}
		lines[sy] = insertStyledLine(lines[sy], m.menu.x, m.menu.w, pr)
	}
	return strings.Join(lines, "\n")
}

// insertStyledLine overlays one popup row (exactly cover cells wide) onto a
// styled content line at visible column col. Escape sequences are hopped over
// while visible cells are counted; sequences whose span falls inside the
// covered region are dropped. The tail right of the block is re-emitted with
// the SGR state that was active at the block's right edge restored in front of
// it, so runs spanning the block keep their original foreground/background
// instead of being repainted with a flat color.
func insertStyledLine(line string, col, cover int, popup string) string {
	base := lipgloss.NewStyle().Background(t.Background).Foreground(t.Foreground)
	var before, after []rune
	part := 0 // 0 before / 1 covered / 2 after
	vis := 0
	i := 0
	in := []rune(line)
	var fg, bg string
	restore := ""
	for i < len(in) {
		if in[i] == '\x1b' {
			j := i + 1
			for j < len(in) {
				if c := in[j]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
				j++
			}
			if j >= len(in) {
				break
			}
			parseSGRState(string(in[i:j+1]), &fg, &bg)
			switch part {
			case 0:
				before = append(before, in[i:j+1]...)
			case 2:
				after = append(after, in[i:j+1]...)
			}
			i = j + 1
			continue
		}
		if part == 0 && vis == col {
			part = 1
		}
		switch part {
		case 0:
			before = append(before, in[i])
		case 2:
			after = append(after, in[i])
		}
		vis++
		i++
		if part == 1 && vis == col+cover {
			restore = sgrRestore(fg, bg)
			part = 2
		}
	}
	if part == 0 {
		// Line shorter than the insertion column: extend it with blank
		// background cells so the popup block still lands at col.
		return line + base.Render(strings.Repeat(" ", col-vis)) + popup
	}
	return string(before) + popup + restore + string(after)
}

// parseSGRState tracks the fg/bg truecolor state the terminal would be in
// after seq, so a splice can restore the exact style that was active at a
// given column. Non-SGR escapes are ignored.
func parseSGRState(seq string, fg, bg *string) {
	if len(seq) < 3 || seq[1] != '[' || seq[len(seq)-1] != 'm' {
		return
	}
	params := strings.Split(seq[2:len(seq)-1], ";")
	for i := 0; i < len(params); i++ {
		switch params[i] {
		case "0":
			*fg, *bg = "", ""
		case "39":
			*fg = ""
		case "49":
			*bg = ""
		case "38", "48":
			if i+4 < len(params) && params[i+1] == "2" {
				s := params[i] + ";2;" + params[i+2] + ";" + params[i+3] + ";" + params[i+4]
				if params[i] == "38" {
					*fg = s
				} else {
					*bg = s
				}
				i += 4
			}
		}
	}
}

// sgrRestore builds an SGR sequence re-asserting the tracked fg/bg state, or
// the empty string when both are default.
func sgrRestore(fg, bg string) string {
	switch {
	case fg == "" && bg == "":
		return ""
	case bg == "":
		return "\x1b[" + fg + "m"
	case fg == "":
		return "\x1b[" + bg + "m"
	default:
		return "\x1b[" + fg + ";" + bg + "m"
	}
}
