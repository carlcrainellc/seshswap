// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV2} from "../src/XmrSwapV2.sol";
import {Token, FalseToken, ReentrantToken, BombToken, RejectEth, UsdtLike} from "./Tokens.sol";

contract XmrSwapV2Test is Test {
    XmrSwapV2 c;
    address alice = address(0xA11CE);
    uint256 bobPk = 0xB0B5EC;          // test-only EOA key (signs relayed claims)
    address bob;
    address relayer = address(0x7E1A7);
    // test-only ed25519 key shares (secret, LE-encoded public key) from swapd/cmd/edvec
    uint256 constant BOB_S = 0x088fba2ab5511efb59a49f0f8e1ceceeb302ad1f60a4007702607c789719eaf5;
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_S = 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;
    uint64 constant D1 = 1 hours;
    uint64 constant D2 = 2 hours;
    uint256 constant T0 = 1_000_000;

    function setUp() public {
        c = new XmrSwapV2();
        bob = vm.addr(bobPk);
        vm.deal(alice, 100 ether);
        vm.warp(T0);
    }

    function _swap(address asset, uint256 value, bytes32 salt) internal view returns (XmrSwapV2.Swap memory) {
        return XmrSwapV2.Swap(alice, bob, BOB_K, ALICE_K, uint64(block.timestamp) + D1,
                              uint64(block.timestamp) + D1 + D2, asset, value, salt);
    }

    function lockEth() internal returns (XmrSwapV2.Swap memory s, bytes32 id) {
        s = _swap(address(0), 1 ether, bytes32(uint256(1)));
        vm.prank(alice);
        id = c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, D2, address(0), 1 ether, bytes32(uint256(1)));
        assertEq(id, c.swapId(s));
    }

    function st(bytes32 id) internal view returns (uint256) { return uint256(c.status(id)); }

    function sign(bytes32 id, address to, address rel, uint256 fee) internal view returns (uint8 v, bytes32 r, bytes32 s) {
        return vm.sign(bobPk, c.claimDigest(id, to, rel, fee));
    }

    // ------------------------------------------------------------ lock
    function test_LockStoresOnlyStatusAndEmitsSwap() public {
        XmrSwapV2.Swap memory s = _swap(address(0), 1 ether, bytes32(uint256(1)));
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV2.Locked(c.swapId(s), s);
        (, bytes32 id) = lockEth();
        assertEq(st(id), 1);
        assertEq(address(c).balance, 1 ether);
    }

    function test_LockRejectsBadParams() public {
        vm.startPrank(alice);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(address(0), BOB_K, ALICE_K, D1, D2, address(0), 1, 0);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 0}(bob, BOB_K, ALICE_K, D1, D2, address(0), 0, 0);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, 0, D2, address(0), 1, 0);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, 0, address(0), 1, 0);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, BOB_K, D1, D2, address(0), 1, 0);          // same key
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(bob, 1, ALICE_K, D1, D2, address(0), 1, 0);            // identity point
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 2}(bob, BOB_K, ALICE_K, D1, D2, address(0), 1, 0);        // msg.value mismatch
        vm.expectRevert();
        c.lock{value: 1}(bob, BOB_K, ALICE_K, type(uint64).max, D2, address(0), 1, 0); // overflow
        vm.stopPrank();
    }

    function test_LockDuplicateReverts() public {
        lockEth();
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.Exists.selector);
        c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, D2, address(0), 1 ether, bytes32(uint256(1)));
    }

    // ------------------------------------------------------------ happy paths
    function test_ReadyThenClaim() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        assertEq(st(id), 2);
        address to = address(0xF2E5);
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV2.Claimed(id, BOB_S, to, address(0), 0);
        vm.prank(bob);
        c.claim(s, BOB_S, to);
        assertEq(to.balance, 1 ether);
        assertEq(st(id), 3);
        assertEq(address(c).balance, 0);
    }

    function test_ClaimAfterT1WithoutReady() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        vm.warp(s.t1 - 1);
        vm.prank(bob);
        vm.expectRevert(XmrSwapV2.WrongTime.selector);
        c.claim(s, BOB_S, bob);
        vm.warp(s.t1);
        vm.prank(bob);
        c.claim(s, BOB_S, bob);
        assertEq(bob.balance, 1 ether);
        assertEq(st(id), 3);
    }

    function test_RefundEarly() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV2.Refunded(id, ALICE_S);
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(alice.balance, 100 ether);
        assertEq(st(id), 4);
    }

    function test_RefundAfterT2FromPendingAndReady() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        vm.warp(s.t2 - 1);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.WrongTime.selector);
        c.refund(s, ALICE_S);
        vm.warp(s.t2);
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(st(id), 4);

        XmrSwapV2.Swap memory s2 = _swap(address(0), 1 ether, bytes32(uint256(2)));
        vm.prank(alice);
        c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, D2, address(0), 1 ether, bytes32(uint256(2)));
        vm.warp(s2.t2 + 1000 days);
        vm.prank(alice);
        c.refund(s2, ALICE_S);
        assertEq(alice.balance, 100 ether);
    }

    // ------------------------------------------------------------ window edges (exclusive windows)
    function test_WindowEdges() public {
        (XmrSwapV2.Swap memory s,) = lockEth();
        // t1 - 1: owner can still refund (checked via snapshot) and setReady
        vm.warp(s.t1 - 1);
        uint256 snap = vm.snapshotState();
        vm.prank(alice);
        c.refund(s, ALICE_S);
        vm.revertToState(snap);
        // at t1: setReady and early refund are closed
        vm.warp(s.t1);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.WrongTime.selector);
        c.setReady(s);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.WrongTime.selector);
        c.refund(s, ALICE_S);
        // at t2: claim closed, refund open
        vm.warp(s.t2);
        vm.prank(bob);
        vm.expectRevert(XmrSwapV2.WrongTime.selector);
        c.claim(s, BOB_S, bob);
        vm.prank(alice);
        c.refund(s, ALICE_S);
    }

    function test_ReadyBlocksEarlyRefund() public {
        (XmrSwapV2.Swap memory s,) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.WrongTime.selector);
        c.refund(s, ALICE_S);
    }

    /// At every second in the swap's life at most one party has a usable action.
    function testFuzz_NoOverlappingWindows(uint256 dt, bool ready) public {
        (XmrSwapV2.Swap memory s,) = lockEth();
        if (ready) { vm.prank(alice); c.setReady(s); }
        dt = bound(dt, 0, D1 + D2 + 10 days);
        vm.warp(T0 + dt);
        uint256 snap = vm.snapshotState();
        vm.prank(bob);
        (bool claimOk,) = address(c).call(abi.encodeCall(XmrSwapV2.claim, (s, BOB_S, bob)));
        vm.revertToState(snap);
        vm.prank(alice);
        (bool refundOk,) = address(c).call(abi.encodeCall(XmrSwapV2.refund, (s, ALICE_S)));
        assertTrue(!(claimOk && refundOk), "both parties could act");
        assertTrue(claimOk || refundOk, "nobody could act");
    }

    // ------------------------------------------------------------ authorization & secrets
    function test_AuthAndSecrets() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        vm.expectRevert(XmrSwapV2.NotAllowed.selector);
        c.setReady(s);
        vm.prank(bob);
        vm.expectRevert(XmrSwapV2.NotAllowed.selector);
        c.refund(s, ALICE_S);
        vm.prank(alice);
        c.setReady(s);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.NotAllowed.selector);
        c.claim(s, BOB_S, alice);
        vm.startPrank(bob);
        vm.expectRevert(XmrSwapV2.BadSecret.selector);
        c.claim(s, ALICE_S, bob);
        vm.expectRevert(XmrSwapV2.BadSecret.selector);
        c.claim(s, 0, bob);
        vm.expectRevert(XmrSwapV2.BadSecret.selector);
        c.claim(s, BOB_S + 0x1000000000000000000000000000000014def9dea2f79cd65812631a5cf5d3ed, bob); // s + l
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.claim(s, BOB_S, address(0));
        c.claim(s, BOB_S, bob);
        vm.expectRevert(XmrSwapV2.WrongStatus.selector);
        c.claim(s, BOB_S, bob);
        vm.stopPrank();
        vm.warp(s.t2);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.WrongStatus.selector);
        c.refund(s, ALICE_S);
        assertEq(st(id), 3);
    }

    function test_TamperedSwapStructRejected() public {
        (XmrSwapV2.Swap memory s,) = lockEth();
        s.value = 2 ether; // attacker-supplied params hash to an unknown id
        vm.prank(bob);
        vm.warp(s.t1);
        vm.expectRevert(XmrSwapV2.WrongStatus.selector);
        c.claim(s, BOB_S, bob);
    }

    // ------------------------------------------------------------ relayed claim (gasless for Bob)
    function test_RelayedClaimPaysFee() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        address to = address(0xF2E5);
        (uint8 v, bytes32 r, bytes32 ss) = sign(id, to, address(0), 0.001 ether);
        vm.prank(relayer);
        c.claimRelayed(s, BOB_S, to, address(0), 0.001 ether, v, r, ss);
        assertEq(to.balance, 0.999 ether);
        assertEq(relayer.balance, 0.001 ether);
        assertEq(bob.balance, 0);
    }

    function test_RelayedClaimBoundRelayer() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        (uint8 v, bytes32 r, bytes32 ss) = sign(id, bob, relayer, 1e15);
        vm.prank(address(0xBAD));
        vm.expectRevert(XmrSwapV2.NotAllowed.selector);
        c.claimRelayed(s, BOB_S, bob, relayer, 1e15, v, r, ss);
        vm.prank(relayer);
        c.claimRelayed(s, BOB_S, bob, relayer, 1e15, v, r, ss);
        assertEq(relayer.balance, 1e15);
    }

    function test_RelayedClaimTamperingRejected() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        (uint8 v, bytes32 r, bytes32 ss) = sign(id, bob, address(0), 1e15);
        vm.startPrank(relayer);
        vm.expectRevert(XmrSwapV2.BadSignature.selector);
        c.claimRelayed(s, BOB_S, relayer, address(0), 1e15, v, r, ss);      // redirect funds
        vm.expectRevert(XmrSwapV2.BadSignature.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 2e15, v, r, ss);          // raise fee
        vm.expectRevert(XmrSwapV2.BadSignature.selector);                  // malleated (s -> n - s)
        c.claimRelayed(s, BOB_S, bob, address(0), 1e15, v == 27 ? 28 : 27, r,
            bytes32(0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141 - uint256(ss)));
        vm.expectRevert(XmrSwapV2.BadSignature.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 1e15, 29, r, ss);
        // signature by someone other than the claimer
        (v, r, ss) = vm.sign(0xBEEF, c.claimDigest(id, bob, address(0), 1e15));
        vm.expectRevert(XmrSwapV2.BadSignature.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 1e15, v, r, ss);
        // fee larger than the swap
        (v, r, ss) = sign(id, bob, address(0), 2 ether);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 2 ether, v, r, ss);
        vm.stopPrank();
    }

    function test_RelayedSignatureNotReplayableAcrossSwapsOrContracts() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        (uint8 v, bytes32 r, bytes32 ss) = sign(id, bob, address(0), 0);
        // second swap with a different salt
        XmrSwapV2.Swap memory s2 = _swap(address(0), 1 ether, bytes32(uint256(9)));
        vm.prank(alice);
        c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, D2, address(0), 1 ether, bytes32(uint256(9)));
        vm.warp(s.t1);
        vm.expectRevert(XmrSwapV2.BadSignature.selector);
        c.claimRelayed(s2, BOB_S, bob, address(0), 0, v, r, ss);
        // same swap params on another deployment: different domain separator
        XmrSwapV2 c2 = new XmrSwapV2();
        assertTrue(c2.domainSeparator() != c.domainSeparator());
        c.claimRelayed(s, BOB_S, bob, address(0), 0, v, r, ss);
        // chain fork: domain separator is recomputed
        bytes32 before = c.domainSeparator();
        vm.chainId(999);
        assertTrue(c.domainSeparator() != before);
    }

    // ------------------------------------------------------------ pull-payment fallback
    function test_RejectingRecipientDefersPayout() public {
        (XmrSwapV2.Swap memory s, bytes32 id) = lockEth();
        address rej = address(new RejectEth());
        vm.warp(s.t1);
        vm.prank(bob);
        c.claim(s, BOB_S, rej); // must not revert. (A reverted claim would still put the secret on-chain:
                                // calldata is public once broadcast, so a revert keeps nothing private.)
        assertEq(st(id), 3);
        assertEq(c.owed(address(0), rej), 1 ether);
        vm.prank(rej);
        c.withdraw(address(0), bob);
        assertEq(bob.balance, 1 ether);
        vm.prank(rej);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.withdraw(address(0), bob);
    }

    // ------------------------------------------------------------ ERC-20
    function lockToken(address tok, uint256 amt) internal returns (XmrSwapV2.Swap memory s, bytes32 id) {
        Token(tok).mint(alice, amt);
        vm.prank(alice);
        Token(tok).approve(address(c), amt);
        s = _swap(tok, amt, bytes32(uint256(7)));
        vm.prank(alice);
        id = c.lock(bob, BOB_K, ALICE_K, D1, D2, tok, amt, bytes32(uint256(7)));
    }

    function test_Erc20HappyAndNoReturnToken() public {
        Token t = new Token();
        t.setNoReturn(true);
        (XmrSwapV2.Swap memory s,) = lockToken(address(t), 1000e6);
        vm.prank(alice);
        c.setReady(s);
        (uint8 v, bytes32 r, bytes32 ss) = sign(c.swapId(s), bob, address(0), 1e6);
        vm.prank(relayer);
        c.claimRelayed(s, BOB_S, bob, address(0), 1e6, v, r, ss);
        assertEq(t.balanceOf(bob), 999e6);
        assertEq(t.balanceOf(relayer), 1e6);
    }

    function test_Erc20FeeOnTransferRejected() public {
        Token t = new Token();
        t.setFee(100);
        t.mint(alice, 1000);
        vm.prank(alice);
        t.approve(address(c), 1000);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock(bob, BOB_K, ALICE_K, D1, D2, address(t), 1000, 0);
    }

    function test_Erc20EthSentWithTokenRejected() public {
        Token t = new Token();
        t.mint(alice, 1000);
        vm.prank(alice);
        t.approve(address(c), 1000);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, D2, address(t), 1000, 0);
    }

    function test_Erc20NoCodeAssetRejected() public {
        vm.prank(alice);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock(bob, BOB_K, ALICE_K, D1, D2, address(0xdead), 1000, 0);
    }

    function test_Erc20BlacklistedRecipientDefersThenWithdrawElsewhere() public {
        Token t = new Token();
        (XmrSwapV2.Swap memory s, bytes32 id) = lockToken(address(t), 500);
        t.setBlocked(bob, true);
        vm.warp(s.t1);
        vm.prank(bob);
        c.claim(s, BOB_S, bob);
        assertEq(st(id), 3);
        assertEq(c.owed(address(t), bob), 500);
        vm.prank(bob);
        c.withdraw(address(t), address(0xC0FFEE));
        assertEq(t.balanceOf(address(0xC0FFEE)), 500);
    }

    function test_Erc20FalseReturnDefers() public {
        FalseToken t = new FalseToken();
        (XmrSwapV2.Swap memory s,) = lockToken(address(t), 500);
        t.setFail(true);
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(c.owed(address(t), alice), 500);
    }

    function test_Erc20ReturnBombHandled() public {
        BombToken t = new BombToken();
        (XmrSwapV2.Swap memory s,) = lockToken(address(t), 500);
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(t.balanceOf(alice), 500);
    }

    function test_ReentrancyBlocked() public {
        ReentrantToken t = new ReentrantToken();
        (XmrSwapV2.Swap memory s,) = lockToken(address(t), 500);
        // a second swap the attacker tries to refund from inside the token callback
        t.mint(alice, 500);
        vm.prank(alice);
        t.approve(address(c), 500);
        XmrSwapV2.Swap memory s2 = _swap(address(t), 500, bytes32(uint256(8)));
        vm.prank(alice);
        c.lock(bob, BOB_K, ALICE_K, D1, D2, address(t), 500, bytes32(uint256(8)));
        t.arm(address(c), abi.encodeCall(XmrSwapV2.refund, (s2, ALICE_S)));
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertTrue(t.reentered());
        assertTrue(!t.reentrySucceeded());
        assertEq(uint256(c.status(c.swapId(s2))), 1);
        assertEq(t.balanceOf(alice), 500);
        assertEq(t.balanceOf(address(c)), 500);
    }

    function test_ReentrancyFromOwnerContractBlocked() public {
        ReentrantOwner o = new ReentrantOwner(c);
        vm.deal(address(o), 2 ether);
        (XmrSwapV2.Swap memory s1, XmrSwapV2.Swap memory s2) = o.setup(BOB_K, ALICE_K, bob);
        o.go(s1, s2, ALICE_S);
        assertTrue(o.attempted());
        assertEq(o.reentryError(), XmrSwapV2.Reentrancy.selector);
        assertEq(uint256(c.status(c.swapId(s2))), 1);
        assertEq(address(c).balance, 1 ether);
    }

    function test_SetReadyReentrancyFromOwnerContractBlocked() public {
        ReentrantOwner o = new ReentrantOwner(c);
        o.useSetReady();
        vm.deal(address(o), 2 ether);
        (XmrSwapV2.Swap memory s1, XmrSwapV2.Swap memory s2) = o.setup(BOB_K, ALICE_K, bob);
        o.go(s1, s2, ALICE_S);
        assertTrue(o.attempted());
        assertEq(o.reentryError(), XmrSwapV2.Reentrancy.selector);
        assertEq(uint256(c.status(c.swapId(s2))), 1); // still PENDING
    }

    // ------------------------------------------------------------ minimum windows
    function test_LockEnforcesMinimumWindows() public {
        uint64 m1 = c.MIN_D1();
        uint64 m2 = c.MIN_D2();
        assertEq(m1, 30 minutes);
        assertEq(m2, 30 minutes);
        vm.startPrank(alice);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, 1, D2, address(0), 1, 0);        // 1-second window
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, m1 - 1, D2, address(0), 1, 0);
        vm.expectRevert(XmrSwapV2.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, m2 - 1, address(0), 1, 0);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, m1, m2, address(0), 1, 0);        // exactly the floor is fine
        vm.stopPrank();
    }

    // ------------------------------------------------------------ USDT-style token (no return value in its ABI)
    function test_Erc20UsdtStyleNoBoolLockClaimRefund() public {
        UsdtLike t = new UsdtLike();
        t.mint(alice, 2000);
        vm.startPrank(alice);
        t.approve(address(c), 2000);
        c.lock(bob, BOB_K, ALICE_K, D1, D2, address(t), 1000, bytes32(uint256(31)));
        c.lock(bob, BOB_K, ALICE_K, D1, D2, address(t), 1000, bytes32(uint256(32)));
        vm.stopPrank();
        assertEq(t.balanceOf(address(c)), 2000);
        XmrSwapV2.Swap memory s1 = _swap(address(t), 1000, bytes32(uint256(31)));
        XmrSwapV2.Swap memory s2 = _swap(address(t), 1000, bytes32(uint256(32)));
        vm.prank(alice);
        c.refund(s1, ALICE_S); // early refund pays out with a no-bool transfer
        assertEq(t.balanceOf(alice), 1000);
        vm.warp(s2.t1);
        vm.prank(bob);
        c.claim(s2, BOB_S, bob);
        assertEq(t.balanceOf(bob), 1000);
        assertEq(t.balanceOf(address(c)), 0);
        assertEq(c.owed(address(t), bob), 0);
    }

    // ------------------------------------------------------------ gas snapshots (see DESIGN.md)
    function test_GasProfile() public {
        uint256 g;
        XmrSwapV2.Swap memory s = _swap(address(0), 1 ether, bytes32(uint256(1)));
        vm.prank(alice);
        g = gasleft();
        c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, D2, address(0), 1 ether, bytes32(uint256(1)));
        emit log_named_uint("lock (ETH)", g - gasleft());
        vm.prank(alice);
        g = gasleft();
        c.setReady(s);
        emit log_named_uint("setReady", g - gasleft());
        (uint8 v, bytes32 r, bytes32 ss) = sign(c.swapId(s), bob, address(0), 1e15);
        vm.prank(relayer);
        g = gasleft();
        c.claimRelayed(s, BOB_S, bob, address(0), 1e15, v, r, ss);
        emit log_named_uint("claimRelayed (ETH)", g - gasleft());
        XmrSwapV2.Swap memory s2 = _swap(address(0), 1 ether, bytes32(uint256(2)));
        vm.prank(alice);
        c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, D2, address(0), 1 ether, bytes32(uint256(2)));
        vm.prank(alice);
        g = gasleft();
        c.refund(s2, ALICE_S);
        emit log_named_uint("refund (ETH)", g - gasleft());
    }
}

contract ReentrantOwner {
    XmrSwapV2 c;
    XmrSwapV2.Swap other;
    uint256 sec;
    bool public attempted;
    bool public trySetReady; // re-enter with setReady instead of refund
    bytes4 public reentryError;
    constructor(XmrSwapV2 c_) { c = c_; }
    function useSetReady() external { trySetReady = true; }
    function setup(uint256 bk, uint256 ak, address bob) external
        returns (XmrSwapV2.Swap memory s1, XmrSwapV2.Swap memory s2)
    {
        uint64 t1 = uint64(block.timestamp) + 1 hours;
        c.lock{value: 1 ether}(bob, bk, ak, 1 hours, 2 hours, address(0), 1 ether, bytes32(uint256(1)));
        c.lock{value: 1 ether}(bob, bk, ak, 1 hours, 2 hours, address(0), 1 ether, bytes32(uint256(2)));
        s1 = XmrSwapV2.Swap(address(this), bob, bk, ak, t1, t1 + 2 hours, address(0), 1 ether, bytes32(uint256(1)));
        s2 = XmrSwapV2.Swap(address(this), bob, bk, ak, t1, t1 + 2 hours, address(0), 1 ether, bytes32(uint256(2)));
    }
    function go(XmrSwapV2.Swap calldata s1, XmrSwapV2.Swap calldata s2, uint256 secret) external {
        other = s2;
        sec = secret;
        c.refund(s1, secret);
    }
    receive() external payable {
        if (attempted) return;
        attempted = true;
        if (trySetReady) {
            try c.setReady(other) {} catch (bytes memory err) { reentryError = bytes4(err); }
        } else {
            try c.refund(other, sec) {} catch (bytes memory err) { reentryError = bytes4(err); }
        }
    }
}
