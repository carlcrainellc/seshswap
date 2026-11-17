// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV3} from "../src/XmrSwapV3.sol";
import {RejectEth} from "./Tokens.sol";

/// Random sequences of lock (random premium and grace) / ackLocked / setReady /
/// cancelIntent / claimIntent / claim / claimRelayed / refund / withdraw / warp.
contract HandlerV3 is Test {
    XmrSwapV3 public c;
    uint256 constant BOB_S = 0x088fba2ab5511efb59a49f0f8e1ceceeb302ad1f60a4007702607c789719eaf5;
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_S = 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;
    uint256 constant BOB_PK = 0xB0B5EC;
    address public alice = address(0xA11CE);
    address public bob;
    address public rejecter;
    address public sink = address(0x5111C);

    XmrSwapV3.Swap[] public swaps;
    uint256 public ghostLocked;
    uint256 public ghostPaidOut;     // pushed + withdrawn
    uint256 public ghostOwed;
    bool public terminalChanged;
    bool public doubleAction;
    bool public wrongPremiumDest;
    bool public revealDuringFreeze;  // a reveal succeeded while the other side's freeze should block it
    mapping(bytes32 => bool) public acked;
    mapping(bytes32 => bool) public cancelled;
    uint256 salt;
    uint256 aliceBal0;   // balance snapshots kept in storage (avoids stack-too-deep)
    uint256 sinkBal0;
    uint256 bobBal0;
    uint256 public nClaims;
    uint256 public nRefunds;
    uint256 public nIntents;
    uint256 public nCancels;
    uint256 public nAcks;
    uint256 public nForfeits;

    constructor(XmrSwapV3 c_) {
        c = c_;
        bob = vm.addr(BOB_PK);
        rejecter = address(new RejectEth());
        vm.deal(alice, 1e30);
    }

    function _st(bytes32 id) internal view returns (uint8 st, uint64 d) {
        (XmrSwapV3.Stage s, uint64 dl,) = c.status(id);
        return (uint8(s), dl);
    }

    function lock(uint256 value, uint256 premium, uint64 d1, uint64 d2, uint64 grace) external {
        XmrSwapV3.Swap memory s;
        s.owner = alice;
        s.claimer = bob;
        s.claimKey = BOB_K;
        s.refundKey = ALICE_K;
        s.value = bound(value, 1, 1e24);
        s.premium = bound(premium, 0, 1e22);
        d1 = uint64(bound(d1, c.MIN_D1(), 30 days));
        s.t1 = uint64(block.timestamp) + d1;
        s.t2 = s.t1 + uint64(bound(d2, c.MIN_D2(), 30 days));
        s.grace = uint64(bound(grace, c.MIN_GRACE(), c.MAX_GRACE()));
        s.salt = bytes32(++salt);
        _lock(s, d1);
    }

    function _lock(XmrSwapV3.Swap memory s, uint64 d1) internal {
        vm.prank(alice);
        c.lock{value: s.value + s.premium}(bob, BOB_K, ALICE_K, d1, s.t2 - s.t1, s.grace, address(0), s.value, s.premium, s.salt);
        swaps.push(s);
        ghostLocked += s.value + s.premium;
    }

    function _pick(uint256 i) internal view returns (XmrSwapV3.Swap memory s, bytes32 id, bool ok) {
        if (swaps.length == 0) return (s, id, false);
        s = swaps[i % swaps.length];
        id = c.swapId(s);
        ok = true;
    }

    function _after(bytes32 id, uint8 before) internal {
        (uint8 now_,) = _st(id);
        if ((before == 3 || before == 4) && now_ != before) terminalChanged = true;
    }

    function warp(uint256 dt) external { vm.warp(block.timestamp + bound(dt, 0, 3 days)); }

    function ack(uint256 i) external {
        (XmrSwapV3.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        (uint8 b,) = _st(id);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(BOB_PK, c.ackDigest(id, sink));
        try c.ackLocked(s, sink, v, r, ss) { acked[id] = true; nAcks++; } catch {}
        _after(id, b);
    }

    function setReady(uint256 i) external {
        (XmrSwapV3.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        (uint8 b,) = _st(id);
        vm.prank(alice);
        try c.setReady(s) {} catch {}
        _after(id, b);
    }

    function cancelIntent(uint256 i) external {
        (XmrSwapV3.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        (uint8 b,) = _st(id);
        vm.prank(alice);
        try c.cancelIntent(s) { cancelled[id] = true; nCancels++; } catch {}
        _after(id, b);
    }

    function claimIntent(uint256 i) external {
        (XmrSwapV3.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        (uint8 b, uint64 d) = _st(id);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(BOB_PK, c.intentDigest(id));
        try c.claimIntent(s, v, r, ss) {
            nIntents++;
            if (b == 6 && block.timestamp < d) revealDuringFreeze = true;
        } catch {}
        _after(id, b);
    }

    function claim(uint256 i, bool toRejecter) external {
        (XmrSwapV3.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        (uint8 b, uint64 d) = _st(id);
        address to = toRejecter ? rejecter : bob;
        bobBal0 = bob.balance;
        aliceBal0 = alice.balance;
        vm.prank(bob);
        try c.claim(s, BOB_S, to) {
            if (b == 3 || b == 4) doubleAction = true;
            if (b != 5 || block.timestamp >= d) revealDuringFreeze = true; // only inside its own freeze
            nClaims++;
            if (toRejecter) ghostOwed += s.value; else { ghostPaidOut += s.value; assertEq(bob.balance - bobBal0, s.value); }
            ghostPaidOut += s.premium;
            if (alice.balance - aliceBal0 != s.premium) wrongPremiumDest = true;
        } catch {}
        _after(id, b);
    }

    function claimRelayed(uint256 i, uint256 fee) external {
        (XmrSwapV3.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        fee = bound(fee, 0, s.value);
        (uint8 b, uint64 d) = _st(id);
        aliceBal0 = alice.balance;
        if (_relayed(s, id, fee)) {
            if (b == 3 || b == 4) doubleAction = true;
            if (b != 5 || block.timestamp >= d) revealDuringFreeze = true;
            nClaims++;
            ghostPaidOut += s.value + s.premium;
            if (alice.balance - aliceBal0 != s.premium) wrongPremiumDest = true;
        }
        _after(id, b);
    }

    function _relayed(XmrSwapV3.Swap memory s, bytes32 id, uint256 fee) internal returns (bool) {
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(BOB_PK, c.claimDigest(id, bob, address(0), fee));
        vm.prank(address(0x7E1A7));
        try c.claimRelayed(s, BOB_S, bob, address(0), fee, v, r, ss) { return true; } catch { return false; }
    }

    function refund(uint256 i) external {
        (XmrSwapV3.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        (uint8 b, uint64 d) = _st(id);
        aliceBal0 = alice.balance;
        sinkBal0 = sink.balance;
        vm.prank(alice);
        try c.refund(s, ALICE_S) {
            nRefunds++;
            if (b == 3 || b == 4) doubleAction = true;
            if (b == 5 && block.timestamp < d) revealDuringFreeze = true; // claimer's freeze
            if ((b == 1 || b == 2) && block.timestamp < s.t2) revealDuringFreeze = true; // claimer's window could open
            ghostPaidOut += s.value + s.premium;
            if (b == 6 && acked[id]) {
                nForfeits++;
                if (sink.balance - sinkBal0 != s.premium || alice.balance - aliceBal0 != s.value) wrongPremiumDest = true;
            } else {
                if (sink.balance != sinkBal0 || alice.balance - aliceBal0 != s.value + s.premium) wrongPremiumDest = true;
            }
        } catch {}
        _after(id, b);
    }

    function withdrawRejecter() external {
        uint256 o = c.owed(address(0), rejecter);
        if (o == 0) return;
        vm.prank(rejecter);
        c.withdraw(address(0), bob);
        ghostOwed -= o;
        ghostPaidOut += o;
    }

    function activeValue() external view returns (uint256 sum) {
        for (uint256 i = 0; i < swaps.length; i++) {
            (uint8 st,) = _st(c.swapId(swaps[i]));
            if (st != 3 && st != 4) sum += swaps[i].value + swaps[i].premium;
        }
    }
}

contract XmrSwapV3Invariants is Test {
    XmrSwapV3 c;
    HandlerV3 h;

    function setUp() public {
        vm.warp(1_000_000);
        c = new XmrSwapV3();
        h = new HandlerV3(c);
        targetContract(address(h));
    }

    function afterInvariant() public {
        emit log_named_uint("claims", h.nClaims());
        emit log_named_uint("refunds", h.nRefunds());
        emit log_named_uint("claim intents", h.nIntents());
        emit log_named_uint("cancel intents", h.nCancels());
        emit log_named_uint("acks", h.nAcks());
        emit log_named_uint("premium forfeits", h.nForfeits());
    }

    /// Contract always holds exactly: value + premium of open swaps + deferred payouts.
    function invariant_Solvency() public view {
        assertEq(address(c).balance, h.activeValue() + c.owed(address(0), h.rejecter()));
        assertEq(h.ghostOwed(), c.owed(address(0), h.rejecter()));
    }

    /// Conservation: everything locked is either still locked, owed, or paid out once.
    function invariant_Conservation() public view {
        assertEq(h.ghostLocked(), h.activeValue() + h.ghostOwed() + h.ghostPaidOut());
    }

    /// A finished swap never changes again and is never paid twice.
    function invariant_TerminalIsFinal() public view {
        assertTrue(!h.terminalChanged());
        assertTrue(!h.doubleAction());
    }

    /// A secret is only ever accepted inside its own side's freeze (or after t2 for the owner).
    function invariant_NoRevealDuringOtherSidesFreeze() public view {
        assertTrue(!h.revealDuringFreeze());
    }

    /// The premium goes to the acked address only when a refund settles a cancel after an ack.
    function invariant_PremiumGoesToTheRightParty() public view {
        assertTrue(!h.wrongPremiumDest());
    }
}
