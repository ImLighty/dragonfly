package player

import (
	"fmt"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/block"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/entity"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/entity/action"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/entity/damage"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/entity/physics"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/entity/state"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/event"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/item"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/item/inventory"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/player/bossbar"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/player/chat"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/player/form"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/player/scoreboard"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/player/skin"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/player/title"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/session"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/world"
	"git.jetbrains.space/dragonfly/dragonfly.git/dragonfly/world/gamemode"
	"github.com/go-gl/mathgl/mgl32"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/cmd"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Player is an implementation of a player entity. It has methods that implement the behaviour that players
// need to play in the world.
type Player struct {
	name                      string
	uuid                      uuid.UUID
	xuid                      string
	pos, velocity, yaw, pitch atomic.Value

	gameModeMu sync.RWMutex
	gameMode   gamemode.GameMode

	skin skin.Skin

	sMutex sync.RWMutex
	// s holds the session of the player. This field should not be used directly, but instead,
	// Player.session() should be called.
	s *session.Session

	hMutex sync.RWMutex
	// h holds the current handler of the player. It may be changed at any time by calling the Start method.
	h Handler

	inv, offHand *inventory.Inventory
	heldSlot     *uint32

	sneaking, sprinting, invisible uint32

	speed             atomic.Value
	health, maxHealth atomic.Value
	immunity          atomic.Value
}

// New returns a new initialised player. A random UUID is generated for the player, so that it may be
// identified over network.
func New(name string, skin skin.Skin, pos mgl32.Vec3) *Player {
	p := &Player{
		name:     name,
		h:        NopHandler{},
		uuid:     uuid.New(),
		skin:     skin,
		inv:      inventory.New(36, nil),
		offHand:  inventory.New(1, nil),
		heldSlot: new(uint32),
		gameMode: gamemode.Adventure{},
	}
	p.pos.Store(pos)
	p.velocity.Store(mgl32.Vec3{})
	p.yaw.Store(float32(0.0))
	p.pitch.Store(float32(0.0))
	p.speed.Store(float32(0.1))
	p.health.Store(float32(20))
	p.maxHealth.Store(float32(20))
	p.immunity.Store(time.Now())
	return p
}

// NewWithSession returns a new player for a network session, so that the network session can control the
// player.
// A set of additional fields must be provided to initialise the player with the client's data, such as the
// name and the skin of the player.
func NewWithSession(name, xuid string, uuid uuid.UUID, skin skin.Skin, s *session.Session, pos mgl32.Vec3) *Player {
	p := New(name, skin, pos)
	p.s = s
	p.uuid = uuid
	p.xuid = xuid
	p.skin = skin

	p.inv, p.offHand, p.heldSlot = s.HandleInventories()

	chat.Global.Subscribe(p)
	return p
}

// Name returns the username of the player. If the player is controlled by a client, it is the username of
// the client. (Typically the XBOX Live name)
func (p *Player) Name() string {
	return p.name
}

// UUID returns the UUID of the player. This UUID will remain consistent with an XBOX Live account, and will,
// unlike the name of the player, never change.
// It is therefore recommended to use the UUID over the name of the player. Additionally, it is recommended to
// use the UUID over the XUID because of its standard format.
func (p *Player) UUID() uuid.UUID {
	return p.uuid
}

// XUID returns the XBOX Live user ID of the player. It will remain consistent with the XBOX Live account,
// and will not change in the lifetime of an account.
// The XUID is a number that can be parsed as an int64. No more information on what it represents is
// available, and the UUID should be preferred.
// The XUID returned is empty if the Player is not connected to a network session.
func (p *Player) XUID() string {
	return p.xuid
}

// Skin returns the skin that a player joined with. This skin will be visible to other players that the player
// is shown to.
// If the player was not connected to a network session, a default skin will be set.
func (p *Player) Skin() skin.Skin {
	return p.skin
}

// Handle changes the current handler of the player. As a result, events called by the player will call
// handlers of the Handler passed.
// Handle sets the player's handler to NopHandler if nil is passed.
func (p *Player) Handle(h Handler) {
	p.hMutex.Lock()
	defer p.hMutex.Unlock()

	if h == nil {
		h = NopHandler{}
	}
	p.h = h
}

// Message sends a formatted message to the player. The message is formatted following the rules of
// fmt.Sprintln, however the newline at the end is not written.
func (p *Player) Message(a ...interface{}) {
	p.session().SendMessage(format(a))
}

// SendPopup sends a formatted popup to the player. The popup is shown above the hotbar of the player and
// overwrites/is overwritten by the name of the item equipped.
// The popup is formatted following the rules of fmt.Sprintln without a newline at the end.
func (p *Player) SendPopup(a ...interface{}) {
	p.session().SendPopup(format(a))
}

// SendTip sends a tip to the player. The tip is shown in the middle of the screen of the player.
// The tip is formatted following the rules of fmt.Sprintln without a newline at the end.
func (p *Player) SendTip(a ...interface{}) {
	p.session().SendTip(format(a))
}

// SendTitle sends a title to the player. The title may be configured to change the duration it is displayed
// and the text it shows.
// If non-empty, the subtitle is shown in a smaller font below the title. The same counts for the action text
// of the title, which is shown in a font similar to that of a tip/popup.
func (p *Player) SendTitle(t title.Title) {
	p.session().SetTitleDurations(t.FadeInDuration(), t.Duration(), t.FadeOutDuration())
	p.session().SendTitle(t.Text())
	if t.Subtitle() != "" {
		p.session().SendSubtitle(t.Subtitle())
	}
	if t.ActionText() != "" {
		p.session().SendActionBarMessage(t.ActionText())
	}
}

// SendScoreboard sends a scoreboard to the player. The scoreboard will be present indefinitely until removed
// by the caller.
// SendScoreboard may be called at any time to change the scoreboard of the player.
func (p *Player) SendScoreboard(scoreboard *scoreboard.Scoreboard) {
	p.session().SendScoreboard(scoreboard.Name())
	p.session().SendScoreboardLines(scoreboard.Lines())
}

// RemoveScoreboard removes any scoreboard currently present on the screen of the player. Nothing happens if
// the player has no scoreboard currently active.
func (p *Player) RemoveScoreboard() {
	p.session().RemoveScoreboard()
}

// SendBossBar sends a boss bar to the player, so that it will be shown indefinitely at the top of the
// player's screen.
// The boss bar may be removed by calling Player.RemoveBossBar().
func (p *Player) SendBossBar(bar bossbar.BossBar) {
	p.session().SendBossBar(bar.Text(), bar.HealthPercentage())
}

// RemoveBossBar removes any boss bar currently active on the player's screen. If no boss bar is currently
// present, nothing happens.
func (p *Player) RemoveBossBar() {
	p.session().RemoveBossBar()
}

// Chat writes a message in the global chat (chat.Global). The message is prefixed with the name of the
// player and is formatted following the rules of fmt.Sprintln.
func (p *Player) Chat(msg ...interface{}) {
	if p.Dead() {
		return
	}
	message := format(msg)
	ctx := event.C()
	p.handler().HandleChat(ctx, &message)

	ctx.Continue(func() {
		chat.Global.Printf("<%v> %v\n", p.name, message)
	})
}

// ExecuteCommand executes a command passed as the player. If the command could not be found, or if the usage
// was incorrect, an error message is sent to the player.
func (p *Player) ExecuteCommand(commandLine string) {
	if p.Dead() {
		return
	}
	args := strings.Split(commandLine, " ")
	commandName := strings.TrimPrefix(args[0], "/")

	command, ok := cmd.ByAlias(commandName)
	if !ok {
		output := &cmd.Output{}
		output.Errorf("Unknown command '%v'", commandName)
		p.SendCommandOutput(output)
		return
	}

	ctx := event.C()
	p.handler().HandleCommandExecution(ctx, command, args[1:])
	ctx.Continue(func() {
		command.Execute(strings.TrimPrefix(commandLine, "/"+commandName+" "), p)
	})
}

// Disconnect closes the player and removes it from the world.
// Disconnect, unlike Close, allows a custom message to be passed to show to the player when it is
// disconnected. The message is formatted following the rules of fmt.Sprintln without a newline at the end.
func (p *Player) Disconnect(msg ...interface{}) {
	p.session().Disconnect(format(msg))
	p.close()
}

// Transfer transfers the player to a server at the address passed. If the address could not be resolved, an
// error is returned. If it is returned, the player is closed and transferred to the server.
func (p *Player) Transfer(address string) (err error) {
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return err
	}
	ctx := event.C()
	p.handler().HandleTransfer(ctx, addr)

	ctx.Continue(func() {
		p.session().Transfer(addr.IP, addr.Port)
		err = p.Close()
	})
	return
}

// SendCommandOutput sends the output of a command to the player.
func (p *Player) SendCommandOutput(output *cmd.Output) {
	p.session().SendCommandOutput(output)
}

// SendForm sends a form to the player for the client to fill out. Once the client fills it out, the Submit
// method of the form will be called.
// Note that the client may also close the form instead of filling it out, which will result in the form not
// having its Submit method called at all. Forms should never depend on the player actually filling out the
// form.
func (p *Player) SendForm(f form.Form) {
	p.session().SendForm(f)
}

// ShowCoordinates enables the vanilla coordinates for the player.
func (p *Player) ShowCoordinates() {
	p.session().EnableCoordinates(true)
}

// ShowCoordinates disables the vanilla coordinates for the player.
func (p *Player) HideCoordinates() {
	p.session().EnableCoordinates(false)
}

// SetSpeed sets the speed of the player. The value passed is the blocks/tick speed that the player will then
// obtain.
func (p *Player) SetSpeed(speed float32) {
	p.speed.Store(speed)
}

// Speed returns the speed of the player, returning a value that indicates the blocks/tick speed. The default
// speed of a player is 0.1.
func (p *Player) Speed() float32 {
	return p.speed.Load().(float32)
}

// Health returns the current health of the player. It will always be lower than Player.MaxHealth().
func (p *Player) Health() float32 {
	return p.health.Load().(float32)
}

// MaxHealth returns the maximum amount of health that a player may have. The MaxHealth will always be higher
// than Player.Health().
func (p *Player) MaxHealth() float32 {
	return p.maxHealth.Load().(float32)
}

// SetMaxHealth sets the maximum health of the player. If the current health of the player is higher than the
// new maximum health, the health is set to the new maximum.
// SetMaxHealth panics if the max health passed is 0 or lower.
func (p *Player) SetMaxHealth(health float32) {
	if health <= 0 {
		panic("max health must not be lower than 0")
	}
	p.maxHealth.Store(health)
	if p.Health() > p.MaxHealth() {
		p.health.Store(health)
	}
	p.session().SendHealth(p.Health(), health)
}

// setHealth sets the current health of the player to the health passed.
func (p *Player) setHealth(health float32) {
	p.health.Store(health)
	p.session().SendHealth(health, p.MaxHealth())
}

// Hurt hurts the player for a given amount of damage. The source passed represents the cause of the damage,
// for example damage.SourceEntityAttack if the player is attacked by another entity.
// If the final damage exceeds the health that the player currently has, the player is killed and will have to
// respawn.
// If the damage passed is negative, Hurt will not do anything.
func (p *Player) Hurt(dmg float32, source damage.Source) {
	if p.Dead() || dmg < 0 || !p.survival() {
		return
	}
	if p.Health()-dmg < 0 {
		dmg = p.Health()
	}

	ctx := event.C()
	p.handler().HandleHurt(ctx, &dmg, source)
	ctx.Continue(func() {
		p.setHealth(p.Health() - dmg)

		for _, viewer := range p.World().Viewers(p.Position()) {
			viewer.ViewEntityAction(p, action.Hurt{})
		}
		p.immunity.Store(time.Now().Add(time.Second / 2))
		if p.Dead() {
			p.kill(source)
		}
	})
}

// KnockBack knocks the player back with a given force and height. A source is passed which indicates the
// source of the knockback, typically the position of an attacking entity. The source is used to calculate the
// direction which the entity should be knocked back in.
func (p *Player) KnockBack(src mgl32.Vec3, force, height float32) {
	if p.Dead() || !p.survival() {
		return
	}
	if p.session() == session.Nop {
		// TODO: Implement server-side movement and knock-back.
		return
	}
	velocity := p.Position().Sub(src).Normalize().Mul(force)
	velocity[1] = height

	p.session().SendVelocity(velocity)
}

// Immune checks if the player is currently immune to entity attacks, meaning it was recently attacked.
func (p *Player) AttackImmune() bool {
	return p.immunity.Load().(time.Time).After(time.Now())
}

// survival checks if the player is considered to be survival, meaning either adventure or survival gamemode.
func (p *Player) survival() bool {
	switch p.GameMode().(type) {
	case gamemode.Survival, gamemode.Adventure:
		return true
	}
	return false
}

// Dead checks if the player is considered dead. True is returned if the health of the player is equal to or
// lower than 0.
func (p *Player) Dead() bool {
	return p.Health() <= 0
}

// kill kills the player, clearing its inventories and resetting it to its base state.
func (p *Player) kill(src damage.Source) {
	for _, viewer := range p.World().Viewers(p.Position()) {
		viewer.ViewEntityAction(p, action.Death{})
	}

	p.setHealth(0)
	p.StopSneaking()
	p.StopSprinting()
	p.Inventory().Clear()
	p.offHand.Clear()

	p.handler().HandleDeath(src)
	time.AfterFunc(time.Millisecond*1100, func() {
		if p.session() == session.Nop {
			_ = p.Close()
			return
		}
		if p.Dead() {
			p.SetInvisible()
			// We have an actual client connected to this player: We change its position server side so that in
			// the future, the client won't respawn on the death location when disconnecting. The client should
			// not see the movement itself yet, though.
			p.pos.Store(p.World().Spawn().Vec3())
		}
	})
}

// Respawn respawns the player, so that its health is replenished and it is spawned in the world again.
// Nothing will happen if the player does not have a session connected to it.
func (p *Player) Respawn() {
	if !p.Dead() || p.World() == nil || p.session() == session.Nop {
		return
	}
	pos := p.World().Spawn().Vec3().Add(mgl32.Vec3{0.5, 0, 0.5})
	p.handler().HandleRespawn(&pos)
	p.setHealth(p.MaxHealth())

	p.World().AddEntity(p)
	p.SetVisible()

	p.Teleport(pos)
	p.session().SendRespawn()
}

// StartSprinting makes a player start sprinting, increasing the speed of the player by 30% and making
// particles show up under the feet.
// If the player is sneaking when calling StartSprinting, it is stopped from sneaking.
func (p *Player) StartSprinting() {
	if atomic.LoadUint32(&p.sprinting) == 1 {
		return
	}
	p.StopSneaking()

	atomic.StoreUint32(&p.sprinting, 1)
	p.SetSpeed(p.Speed() * 1.3)

	p.updateState()
}

// StopSprinting makes a player stop sprinting, setting back the speed of the player to its original value.
func (p *Player) StopSprinting() {
	if atomic.LoadUint32(&p.sprinting) == 0 {
		return
	}
	atomic.StoreUint32(&p.sprinting, 0)
	p.SetSpeed(p.Speed() / 1.3)

	p.updateState()
}

// StartSneaking makes a player start sneaking. If the player is already sneaking, StartSneaking will not do
// anything.
// If the player is sprinting while StartSneaking is called, the sprinting is stopped.
func (p *Player) StartSneaking() {
	p.StopSprinting()
	atomic.StoreUint32(&p.sneaking, 1)
	p.updateState()
}

// StopSneaking makes a player stop sneaking if it currently is. If the player is not sneaking, StopSneaking
// will not do anything.
func (p *Player) StopSneaking() {
	atomic.StoreUint32(&p.sneaking, 0)
	p.updateState()
}

// SetInvisible sets the player invisible, so that other players will not be able to see it.
func (p *Player) SetInvisible() {
	atomic.StoreUint32(&p.invisible, 1)
	p.updateState()
}

// SetVisible sets the player visible again, so that other players can see it again. If the player was already
// visible, nothing happens.
func (p *Player) SetVisible() {
	atomic.StoreUint32(&p.invisible, 0)
	p.updateState()
}

// Inventory returns the inventory of the player. This inventory holds the items stored in the normal part of
// the inventory and the hotbar. It also includes the item in the main hand as returned by Player.HeldItems().
func (p *Player) Inventory() *inventory.Inventory {
	return p.inv
}

// HeldItems returns the items currently held in the hands of the player. The first item stack returned is the
// one held in the main hand, the second is held in the off-hand.
// If no item was held in a hand, the stack returned has a count of 0. Stack.Empty() may be used to check if
// the hand held anything.
func (p *Player) HeldItems() (mainHand, offHand item.Stack) {
	offHand, _ = p.offHand.Item(0)
	mainHand, _ = p.inv.Item(int(atomic.LoadUint32(p.heldSlot)))
	return mainHand, offHand
}

// SetHeldItems sets items to the main hand and the off-hand of the player. The Stacks passed may be empty
// (Stack.Empty()) to clear the held item.
func (p *Player) SetHeldItems(mainHand, offHand item.Stack) {
	_ = p.inv.SetItem(int(atomic.LoadUint32(p.heldSlot)), mainHand)
	_ = p.offHand.SetItem(0, offHand)

	for _, viewer := range p.World().Viewers(p.Position()) {
		viewer.ViewEntityItems(p)
	}
}

// SetGameMode sets the game mode of a player. The game mode specifies the way that the player can interact
// with the world that it is in.
func (p *Player) SetGameMode(mode gamemode.GameMode) {
	p.gameModeMu.Lock()
	p.gameMode = mode
	p.gameModeMu.Unlock()
	p.session().SendGameMode(mode)
}

// GameMode returns the current game mode assigned to the player. If not changed, the game mode returned will
// be the same as that of the world that the player spawns in.
// The game mode may be changed using Player.SetGameMode().
func (p *Player) GameMode() gamemode.GameMode {
	p.gameModeMu.RLock()
	mode := p.gameMode
	p.gameModeMu.RUnlock()
	return mode
}

// BreakBlock makes the player break a block in the world at a position passed. If the player is unable to
// reach the block passed, the method returns immediately.
func (p *Player) BreakBlock(pos world.BlockPos) {
	if !p.canReach(pos.Vec3().Add(mgl32.Vec3{0.5, 0.5, 0.5})) {
		return
	}
	ctx := event.C()
	p.handler().HandleBlockBreak(ctx, pos)

	ctx.Continue(func() {
		p.swingArm()
		p.World().BreakBlock(pos)
	})
	ctx.Stop(func() {
		b := p.World().Block(pos)
		// Set back the block to make sure the client sees it like that again.
		p.World().SetBlock(pos, b)
	})
}

// UseItem uses the item currently held in the player's main hand in the air. Generally, nothing happens,
// unless the held item implements the item.Usable interface, in which case it will be activated.
// This generally happens for items such as throwable items like snowballs.
func (p *Player) UseItem() {
	if !p.canReach(p.Position()) {
		return
	}
	i, _ := p.HeldItems()
	ctx := event.C()
	p.handler().HandleItemUse(ctx)

	ctx.Continue(func() {
		usable, ok := i.Item().(item.Usable)
		if !ok {
			// The item wasn't usable, so we can stop doing anything right away.
			return
		}
		usable.Use(p.World(), p)

		// We only swing the player's arm if the item held actually does something. If it doesn't, there is no
		// reason to swing the arm.
		p.swingArm()
	})
}

// UseItemOnBlock uses the item held in the main hand of the player on a block at the position passed. The
// player is assumed to have clicked the face passed with the relative click position clickPos.
// If the item could not be used successfully, for example when the position is out of range, the method
// returns immediately.
func (p *Player) UseItemOnBlock(pos world.BlockPos, face world.Face, clickPos mgl32.Vec3) {
	if !p.canReach(pos.Vec3().Add(mgl32.Vec3{0.5, 0.5, 0.5})) {
		return
	}
	i, _ := p.HeldItems()

	ctx := event.C()
	p.handler().HandleItemUseOnBlock(ctx, pos, face, clickPos)

	ctx.Continue(func() {
		clickedBlock := p.World().Block(pos)

		if activatable, ok := clickedBlock.(block.Activatable); ok {
			// If a player is sneaking, it will not activate the block clicked, unless it is not holding any
			// items, in which the block will activated as usual.
			if atomic.LoadUint32(&p.sneaking) == 0 || i.Empty() {
				p.swingArm()
				// The block was activated: Blocks such as doors must always have precedence over the item being
				// used.
				activatable.Activate(pos, face, p.World(), p)
				return
			}
		}
		if i.Empty() {
			return
		}
		p.swingArm()

		if usableOnBlock, ok := i.Item().(item.UsableOnBlock); ok {
			// The item does something when used on a block.
			usableOnBlock.UseOnBlock(pos, face, clickPos, p.World(), p)
		} else if b, ok := i.Item().(world.Block); ok {
			// The item IS a block, meaning it is being placed.
			placedPos := pos.Side(face)
			existing := p.World().Block(placedPos)
			if _, ok := existing.(block.Air); !ok {
				return
			}
			p.World().PlaceBlock(placedPos, b)
		}
	})
	ctx.Stop(func() {
		if _, ok := i.Item().(world.Block); ok {
			placedPos := pos.Side(face)
			existing := p.World().Block(placedPos)
			// Always put back the block so that the client sees it there again.
			p.World().SetBlock(placedPos, existing)
		}
	})
}

// UseItemOnEntity uses the item held in the main hand of the player on the entity passed, provided it is
// within range of the player.
// If the item held in the main hand of the player does nothing when used on an entity, nothing will happen.
func (p *Player) UseItemOnEntity(e world.Entity) {
	if !p.canReach(e.Position()) {
		return
	}
	i, _ := p.HeldItems()

	ctx := event.C()
	p.handler().HandleItemUseOnEntity(ctx, e)

	ctx.Continue(func() {
		if usableOnEntity, ok := i.Item().(item.UsableOnEntity); ok {
			usableOnEntity.UseOnEntity(e, e.World(), p)
			p.swingArm()
		}
	})
}

// AttackEntity uses the item held in the main hand of the player to attack the entity passed, provided it is
// within range of the player.
// The damage dealt to the entity will depend on the item held by the player and any effects the player may
// have.
// If the player cannot reach the entity at its position, the method returns immediately.
func (p *Player) AttackEntity(e world.Entity) {
	if !p.canReach(e.Position()) {
		return
	}
	i, _ := p.HeldItems()

	ctx := event.C()
	p.handler().HandleAttackEntity(ctx, e)
	ctx.Continue(func() {
		p.swingArm()
		living, ok := e.(entity.Living)
		if !ok {
			return
		}
		if living.AttackImmune() {
			return
		}
		living.Hurt(i.AttackDamage(), damage.SourceEntityAttack{Attacker: p})
		living.KnockBack(p.Position(), 0.5, 0.3)
	})
}

// Teleport teleports the player to a target position in the world. Unlike Move, it immediately changes the
// position of the player, rather than showing an animation.
func (p *Player) Teleport(pos mgl32.Vec3) {
	// Generally it is expected you are teleported to the middle of the block.
	pos = pos.Add(mgl32.Vec3{0.5, 0, 0.5})

	ctx := event.C()
	p.handler().HandleTeleport(ctx, pos)
	ctx.Continue(func() {
		p.teleport(pos)
	})
}

// teleport teleports the player to a target position in the world. It does not call the handler of the
// player.
func (p *Player) teleport(pos mgl32.Vec3) {
	for _, v := range p.World().Viewers(p.Position()) {
		v.ViewEntityTeleport(p, pos)
	}
	p.pos.Store(pos)
}

// Move moves the player from one position to another in the world, by adding the delta passed to the current
// position of the player.
func (p *Player) Move(deltaPos mgl32.Vec3) {
	if p.Dead() || deltaPos.ApproxEqual(mgl32.Vec3{}) {
		return
	}

	ctx := event.C()
	p.handler().HandleMove(ctx, p.Position().Add(deltaPos), p.Yaw(), p.Pitch())
	ctx.Continue(func() {
		for _, v := range p.World().Viewers(p.Position()) {
			v.ViewEntityMovement(p, deltaPos, 0, 0)
		}
		p.pos.Store(p.Position().Add(deltaPos))
	})
	ctx.Stop(func() {
		p.teleport(p.Position())
	})
}

// Rotate rotates the player, adding deltaYaw and deltaPitch to the respective values.
func (p *Player) Rotate(deltaYaw, deltaPitch float32) {
	if p.Dead() || (mgl32.FloatEqual(deltaYaw, 0) && mgl32.FloatEqual(deltaPitch, 0)) {
		return
	}

	p.handler().HandleMove(event.C(), p.Position(), p.Yaw()+deltaYaw, p.Pitch()+deltaPitch)

	// Cancelling player rotation is rather scuffed, so we don't do that.
	for _, v := range p.World().Viewers(p.Position()) {
		v.ViewEntityMovement(p, mgl32.Vec3{}, deltaYaw, deltaPitch)
	}
	p.yaw.Store(p.Yaw() + deltaYaw)
	p.pitch.Store(p.Pitch() + deltaPitch)
}

// Facing returns the horizontal direction that the player is facing.
func (p *Player) Facing() world.Face {
	return entity.Facing(p)
}

// World returns the world that the player is currently in.
func (p *Player) World() *world.World {
	w, _ := world.OfEntity(p)
	return w
}

// Position returns the current position of the player. It may be changed as the player moves or is moved
// around the world.
func (p *Player) Position() mgl32.Vec3 {
	return p.pos.Load().(mgl32.Vec3)
}

// Yaw returns the yaw of the entity. This is horizontal rotation (rotation around the vertical axis), and
// is 0 when the entity faces forward.
func (p *Player) Yaw() float32 {
	return p.yaw.Load().(float32)
}

// Pitch returns the pitch of the entity. This is vertical rotation (rotation around the horizontal axis),
// and is 0 when the entity faces forward.
func (p *Player) Pitch() float32 {
	return p.pitch.Load().(float32)
}

// Collect makes the player collect the item stack passed, adding it to the inventory.
func (p *Player) Collect(s item.Stack) (n int) {
	n, _ = p.Inventory().AddItem(s)
	return
}

// OpenBlockContainer opens a block container, such as a chest or a shulker box, at the position passed. If
// no container was present at that location, OpenBlockContainer does nothing.
// OpenBlockContainer will also do nothing if the player has no session connected to it.
func (p *Player) OpenBlockContainer(pos world.BlockPos) {
	if p.session() == session.Nop {
		return
	}
	p.session().OpenBlockContainer(pos)
}

// Velocity returns the current velocity of the player.
func (p *Player) Velocity() mgl32.Vec3 {
	// TODO: Implement server-side movement of player entities.
	return p.velocity.Load().(mgl32.Vec3)
}

// SetVelocity sets the velocity of the player.
func (p *Player) SetVelocity(v mgl32.Vec3) {
	// TODO: Implement server-side movement of player entities.
	p.velocity.Store(v)
}

// AABB returns the axis aligned bounding box of the player.
func (p *Player) AABB() []physics.AABB {
	switch {
	case atomic.LoadUint32(&p.sneaking) == 1:
		return []physics.AABB{physics.NewAABB(mgl32.Vec3{-0.3, 0, -0.3}, mgl32.Vec3{0.3, 1.65, 0.3})}
	default:
		return []physics.AABB{physics.NewAABB(mgl32.Vec3{-0.3, 0, -0.3}, mgl32.Vec3{0.3, 1.8, 0.3})}
	}
}

// State returns the current state of the player. Types from the `entity/state` package are returned
// depending on what the player is currently doing.
func (p *Player) State() (s []state.State) {
	if atomic.LoadUint32(&p.sneaking) == 1 {
		s = append(s, state.Sneaking{})
	}
	if atomic.LoadUint32(&p.sprinting) == 1 {
		s = append(s, state.Sprinting{})
	}
	if atomic.LoadUint32(&p.invisible) == 1 {
		s = append(s, state.Invisible{})
	}
	// TODO: Only set the player as breathing when it is above water.
	s = append(s, state.Breathing{})
	return
}

// updateState updates the state of the player to all viewers of the player.
func (p *Player) updateState() {
	for _, v := range p.World().Viewers(p.Position()) {
		v.ViewEntityState(p, p.State())
	}
}

// swingArm makes the player swing its arm.
func (p *Player) swingArm() {
	if p.Dead() {
		return
	}
	for _, v := range p.World().Viewers(p.Position()) {
		v.ViewEntityAction(p, action.SwingArm{})
	}
}

// Close closes the player and removes it from the world.
// Close disconnects the player with a 'Connection closed.' message. Disconnect should be used to disconnect a
// player with a custom message.
func (p *Player) Close() error {
	p.session().Disconnect("Connection closed.")
	p.close()
	return nil
}

// canReach checks if a player can reach a position with its current range. The range depends on if the player
// is either survival or creative mode.
func (p *Player) canReach(pos mgl32.Vec3) bool {
	const (
		eyeHeight     = 1.62
		creativeRange = 13.0
		survivalRange = 7.0
	)
	eyes := p.Position().Add(mgl32.Vec3{0, eyeHeight})

	if _, ok := p.GameMode().(gamemode.Creative); ok {
		return world.Distance(eyes, pos) <= creativeRange
	}
	return world.Distance(eyes, pos) <= survivalRange && !p.Dead()
}

// close closed the player without disconnecting it. It executes code shared by both the closing and the
// disconnecting of players.
func (p *Player) close() {
	p.handler().HandleQuit()

	p.Handle(NopHandler{})
	chat.Global.Unsubscribe(p)

	p.sMutex.Lock()
	s := p.s
	p.s = nil

	// Clear the inventories so that they no longer hold references to the connection.
	_ = p.inv.Close()
	_ = p.offHand.Close()
	p.sMutex.Unlock()

	if s == session.Nop {
		p.World().RemoveEntity(p)
	}
}

// session returns the network session of the player. If it has one, it is returned. If not, a no-op session
// is returned.
func (p *Player) session() *session.Session {
	p.sMutex.RLock()
	s := p.s
	p.sMutex.RUnlock()

	if s == nil {
		return session.Nop
	}
	return s
}

// handler returns the handler of the player.
func (p *Player) handler() Handler {
	p.hMutex.RLock()
	handler := p.h
	p.hMutex.RUnlock()
	return handler
}

// format is a utility function to format a list of values to have spaces between them, but no newline at the
// end, which is typically used for sending messages, popups and tips.
func format(a []interface{}) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintln(a...), "\n"), "\n")
}